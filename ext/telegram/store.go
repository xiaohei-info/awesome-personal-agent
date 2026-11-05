package telegram

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record 是一条已发送的投递。发送幂等靠它：投递 id → 已发出的消息，崩溃重启后不再重发。
type Record struct {
	Ref     string    `json:"ref"` // "<chat>:<message_id>"，同时是 ack 的 external_ref 与回答的 ref
	Chat    int64     `json:"chat"`
	Msg     int       `json:"msg"`
	Options []string  `json:"options,omitempty"`
	Stale   bool      `json:"stale,omitempty"` // 内核回过 409：版本已作废，不再对它报告任何东西
	WebOnly bool      `json:"web_only,omitempty"`
	WebURL  string    `json:"web_url,omitempty"` // web_only 时告诉本人去哪里处理
	At      time.Time `json:"at"`
}

// Store 保存发送记录。
type Store interface {
	Lookup(id string) (Record, bool)
	Save(id string, r Record) error
	// Find 按 Telegram 消息找投递（回答时用）。
	Find(chat int64, msg int) (string, Record, bool)
}

// retention 之后的记录会在下次写入时清掉：投递租约与重试远短于此。
const retention = 30 * 24 * time.Hour

// FileStore 把记录写在一个 JSON 文件里；每次写入先写临时文件并 fsync 再改名，kill -9 不会留下半个文件。
type FileStore struct {
	mu   sync.Mutex
	path string
	recs map[string]Record
}

func OpenFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, recs: map[string]Record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.recs); err != nil {
		return nil, errors.New("state file " + path + " is not valid JSON; fix or remove it (removing it may cause re-sends)")
	}
	return s, nil
}

func (s *FileStore) Lookup(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	return r, ok
}

func (s *FileStore) Find(chat int64, msg int) (string, Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.recs {
		if r.Chat == chat && r.Msg == msg {
			return id, r, true
		}
	}
	return "", Record{}, false
}

func (s *FileStore) Save(id string, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.recs[id]
	s.recs[id] = r
	for k, v := range s.recs {
		if time.Since(v.At) > retention {
			delete(s.recs, k)
		}
	}
	if err := s.flush(); err != nil {
		if had {
			s.recs[id] = prev
		} else {
			delete(s.recs, id)
		}
		return err
	}
	return nil
}

func (s *FileStore) flush() error {
	data, _ := json.Marshal(s.recs)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
