package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func get(h http.Handler, p string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
	return w
}

func TestHandlerServesAssetsAndFallsBackToIndex(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html":    {Data: []byte("<div id=root></div>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	})
	if w := get(h, "/assets/app.js"); w.Code != 200 || !strings.Contains(w.Body.String(), "console.log") {
		t.Fatalf("asset: %d %q", w.Code, w.Body)
	}
	for _, p := range []string{"/", "/decisions/item_1", "/assets"} { // 前端路由与目录都回落到 SPA
		if w := get(h, p); w.Code != 200 || !strings.Contains(w.Body.String(), "id=root") {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body)
		}
	}
}

func TestHandlerWithoutBuildFailsLoudly(t *testing.T) {
	if w := get(handler(fstest.MapFS{}), "/"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

func TestEmbeddedHandlerResponds(t *testing.T) {
	if w := get(Handler(), "/"); w.Code != 200 && w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
}
