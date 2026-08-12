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

	// /manage 仍需鉴权。
	if resp2, _ := http.Get(srv.URL + "/manage/v1/providers"); resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("manage without key status = %d, want 401", resp2.StatusCode)
	} else {
		resp2.Body.Close()
	}
	// 模型列表公开,无需鉴权。
	if resp2, _ := http.Get(srv.URL + "/api/v1/models"); resp2.StatusCode != http.StatusOK {
		t.Errorf("api models without key status = %d, want 200", resp2.StatusCode)
	} else {
		resp2.Body.Close()
	}
	// 其余 /api 端点仍需鉴权(鉴权先于请求体解码,body 可为空)。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("api forward without key status = %d, want 401", resp2.StatusCode)
	}
}
