package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func listServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

// 设置 › 模型的"获取模型"：GET <url>/models 带鉴权头，结果去重排序。
func TestListModels(t *testing.T) {
	var auth, path string
	url := listServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		fmt.Fprint(w, `{"object":"list","data":[{"id":"model-b"},{"id":"model-a"},{"id":"model-b"},{"id":""}]}`)
	})
	ids, truncated, err := (&Client{}).ListModels(context.Background(), url+"/v1/", "sekret-key")
	if err != nil || truncated || strings.Join(ids, ",") != "model-a,model-b" {
		t.Fatalf("ids=%v truncated=%v err=%v", ids, truncated, err)
	}
	if auth != "Bearer sekret-key" || path != "/v1/models" {
		t.Fatalf("auth=%q path=%q", auth, path)
	}
}

func TestListModelsTruncates(t *testing.T) {
	url := listServer(t, func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := 0; i < MaxModels+5; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":"m%04d"}`, i)
		}
		b.WriteString("]}")
		fmt.Fprint(w, b.String())
	})
	ids, truncated, err := (&Client{}).ListModels(context.Background(), url, "")
	if err != nil || !truncated || len(ids) != MaxModels {
		t.Fatalf("len=%d truncated=%v err=%v", len(ids), truncated, err)
	}
}

// 失败如实返回：鉴权失败是 *StatusError（不带密钥），不是列表是 ErrBadResponse，超大响应同样拒绝。
func TestListModelsFailures(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		want func(error) bool
	}{
		"unauthorized": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"bad key sekret-key"}}`)
		}, func(err error) bool {
			var se *StatusError
			return errors.As(err, &se) && se.Code == 401 && !strings.Contains(err.Error(), "sekret-key")
		}},
		"not a list": {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[]}`) },
			func(err error) bool { return errors.Is(err, ErrBadResponse) }},
		"not json": {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) },
			func(err error) bool { return errors.Is(err, ErrBadResponse) }},
		"oversized": {func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"id":"`+strings.Repeat("x", maxListBody)+`"}]}`)
		}, func(err error) bool { return errors.Is(err, ErrBadResponse) }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := (&Client{}).ListModels(context.Background(), listServer(t, c.h), "sekret-key")
			if err == nil || !c.want(err) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestListModelsTimeout(t *testing.T) {
	url := listServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := (&Client{}).ListModels(ctx, url, ""); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err=%v", err)
	}
}
