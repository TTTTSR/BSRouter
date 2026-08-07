package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 内嵌前端(SPA)应无需鉴权即可访问,而 /api 与 /manage 仍强制鉴权。
func TestWebUIServedWithoutAuth(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<html>bsrouter-ui</html>")
			return
		}
		http.NotFound(w, r)
	})
	m := newMgr(t)
	srv := httptest.NewServer(New(m).WithAPIKey("secret").WithWebUI(ui).Handler())
	defer srv.Close()

	// SPA 无需鉴权。
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "<html>bsrouter-ui</html>" {
		t.Errorf("SPA = %d %q", resp.StatusCode, string(b))
	}

	// API 仍需鉴权。
	if resp2, _ := http.Get(srv.URL + "/manage/v1/providers"); resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("manage without key status = %d, want 401", resp2.StatusCode)
	} else {
		resp2.Body.Close()
	}
	if resp2, _ := http.Get(srv.URL + "/api/v1/models"); resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("api without key status = %d, want 401", resp2.StatusCode)
	} else {
		resp2.Body.Close()
	}
}
