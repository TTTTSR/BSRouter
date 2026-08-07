package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"BSRouter/internal/provider"
)

func newMgr(t *testing.T) *provider.Manager {
	t.Helper()
	m, err := provider.NewManager(filepath.Join(t.TempDir(), "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newAPI(t *testing.T, m *provider.Manager) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

const cfgBody = `{"kind":"completion","name":"openai","base_url":"https://api.openai.com","api_key":"sk-secret-1234","models":[{"name":"gpt-4o"}]}`

func TestProvidersCRUD(t *testing.T) {
	srv := newAPI(t, newMgr(t))

	// 新增 -> 201,响应掩码 api_key
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", cfgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	var created provider.Config
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.APIKey != maskKey("sk-secret-1234") {
		t.Errorf("created api_key = %q, want masked %q", created.APIKey, maskKey("sk-secret-1234"))
	}

	// 重复新增 -> 409
	if resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", cfgBody); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate POST status = %d, want 409; body=%s", resp.StatusCode, body)
	}

	// 配置非法 -> 400
	if resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", `{"kind":"nope","name":"x","base_url":"http://x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid POST status = %d, want 400; body=%s", resp.StatusCode, body)
	}

	// 列表 -> 200,含 openai 且掩码
	if resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET list status = %d", resp.StatusCode)
	} else {
		var list []provider.Config
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "openai" || list[0].APIKey != maskKey("sk-secret-1234") {
			t.Errorf("list = %+v", list)
		}
	}

	// 单查 -> 200 且掩码
	if resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/openai", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET one status = %d", resp.StatusCode)
	} else {
		var got provider.Config
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != "https://api.openai.com" || got.APIKey != maskKey("sk-secret-1234") {
			t.Errorf("GET one = %+v", got)
		}
	}

	// 单查不存在 -> 404
	if resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/missing", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET missing status = %d, want 404", resp.StatusCode)
	}

	// 修改 -> 200,路径名生效,body 中的 name 被忽略
	updBody := `{"kind":"completion","name":"ignored","base_url":"https://api.new","api_key":"sk-2","models":[{"name":"gpt-5"}]}`
	if resp, body := doJSON(t, srv, http.MethodPut, "/manage/v1/providers/openai", updBody); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/openai", ""); resp.StatusCode == http.StatusOK {
		var got provider.Config
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "openai" || got.BaseURL != "https://api.new" || got.APIKey != maskKey("sk-2") {
			t.Errorf("after update = %+v", got)
		}
	}

	// 修改不存在 -> 404
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/providers/missing", updBody); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing status = %d, want 404", resp.StatusCode)
	}

	// 删除 -> 204
	if resp, _ := doJSON(t, srv, http.MethodDelete, "/manage/v1/providers/openai", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
	}
	// 再删除 -> 404
	if resp, _ := doJSON(t, srv, http.MethodDelete, "/manage/v1/providers/openai", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE again status = %d, want 404", resp.StatusCode)
	}
	// 删除后单查 -> 404
	if resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/openai", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404", resp.StatusCode)
	}
}

func TestListModels(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "an", BaseURL: "http://a", Models: []provider.ModelConfig{{Name: "claude-3"}, {Name: "claude-sonnet-4-5"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: "http://o", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{Kind: provider.KindResponses, Name: "my-oa", BaseURL: "http://mo", Models: []provider.ModelConfig{{Name: "gpt-5"}}}); err != nil {
		t.Fatal(err)
	}
	// 无模型列表的供应商不应出现在聚合中
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "none", BaseURL: "http://n"}); err != nil {
		t.Fatal(err)
	}

	srv := newAPI(t, m)
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models status = %d", resp.StatusCode)
	}
	var out modelList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	got := make([]string, len(out.Data))
	ownedBy := make([]string, len(out.Data))
	for i, e := range out.Data {
		got[i] = e.ID
		ownedBy[i] = e.OwnedBy
	}
	want := []string{"an@claude-3", "an@claude-sonnet-4-5", "my-oa@gpt-5", "oa@gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("model ids = %v, want %v", got, want)
	}
	wantOwned := []string{"an", "an", "my-oa", "oa"}
	if !reflect.DeepEqual(ownedBy, wantOwned) {
		t.Errorf("owned_by = %v, want %v", ownedBy, wantOwned)
	}
	// 每项 object 字段
	for _, e := range out.Data {
		if e.Object != "model" {
			t.Errorf("entry object = %q, want model", e.Object)
		}
	}
}

// 管理端模型列表(/manage/v1/models)与统一 API 同源:供管理界面用 /manage 鉴权
// 访问,避免管理端依赖下游凭据(受管 key)导致 401。
func TestManageModelsEndpoint(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: "http://o", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /manage/v1/models status = %d; body=%s", resp.StatusCode, body)
	}
	var out modelList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "oa@gpt-4o" || out.Data[0].OwnedBy != "oa" {
		t.Errorf("manage models = %+v", out.Data)
	}
}

func TestListModelsEmpty(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(body, "null") {
		t.Errorf("empty data should be [] not null: %s", body)
	}
	var out modelList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 0 || out.Data == nil {
		t.Errorf("data = %+v, want empty non-nil slice", out.Data)
	}
}

// 掩码值或空 api_key 回填 PUT 时,应保留原真实密钥;显式新密钥才覆盖。
func TestProviderUpdatePreservesKey(t *testing.T) {
	m := newMgr(t)
	srv := newAPI(t, m)
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", cfgBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}

	// GET 拿到的掩码值原样 PUT 回去,只改 base_url。
	_, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/openai", "")
	var got provider.Config
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	putBody := fmt.Sprintf(`{"kind":"completion","name":"openai","base_url":"https://api.new","api_key":%q,"models":[{"name":"gpt-4o"}]}`, got.APIKey)
	if resp, b := doJSON(t, srv, http.MethodPut, "/manage/v1/providers/openai", putBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT masked key status = %d; body=%s", resp.StatusCode, b)
	}
	if stored, _ := m.Get("openai"); stored.Config().APIKey != "sk-secret-1234" {
		t.Errorf("stored api_key after masked PUT = %q, want preserved", stored.Config().APIKey)
	}

	// 空 api_key 同样保留。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/providers/openai",
		`{"kind":"completion","name":"x","base_url":"https://api.new","api_key":"","models":[{"name":"gpt-4o"}]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty key status = %d", resp.StatusCode)
	}
	if stored, _ := m.Get("openai"); stored.Config().APIKey != "sk-secret-1234" {
		t.Errorf("stored api_key after empty PUT = %q, want preserved", stored.Config().APIKey)
	}

	// 显式提供新密钥时才覆盖。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/providers/openai",
		`{"kind":"completion","name":"x","base_url":"https://api.new","api_key":"sk-2","models":[{"name":"gpt-4o"}]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT new key status = %d", resp.StatusCode)
	}
	if stored, _ := m.Get("openai"); stored.Config().APIKey != "sk-2" {
		t.Errorf("stored api_key = %q, want sk-2", stored.Config().APIKey)
	}
}

// 新增时提交掩码值属于复制粘贴错误,应被拒绝。
func TestProviderCreateRejectsMaskedKey(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/providers",
		`{"kind":"completion","name":"p","base_url":"http://x","api_key":"sk-s****1234"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST masked key status = %d, want 400; body=%s", resp.StatusCode, b)
	}
}

// 非 JSON Content-Type 的写请求应被拒绝(防浏览器 CSRF 无预检提交)。
func TestProviderRequireJSONContentType(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/manage/v1/providers", strings.NewReader(cfgBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

// 持久化失败应返回 500,且内存态回滚(不出现"内存有、磁盘无"的漂移)。
func TestProviderPersistFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing", "providers.json") // 目录不存在,写入必然失败
	m, err := provider.NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", cfgBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST status = %d, want 500; body=%s", resp.StatusCode, body)
	}
	_, listBody := doJSON(t, srv, http.MethodGet, "/manage/v1/providers", "")
	if strings.Contains(listBody, "openai") {
		t.Errorf("provider leaked into memory despite save failure: %s", listBody)
	}
}

// 分隔符为 @ 后,不同供应商的合成 id 天然不同,无前缀碰撞;两者都发布。
func TestListModelsDistinctComposites(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: "http://a", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai-gpt", BaseURL: "http://b", Models: []provider.ModelConfig{{Name: "4o"}}}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out modelList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(out.Data))
	for i, e := range out.Data {
		got[i] = e.ID
	}
	// 按 id 排序:openai-gpt@4o("-" 0x2d)先于 openai@gpt-4o("@" 0x40)。
	want := []string{"openai-gpt@4o", "openai@gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
}

// 请求体超过大小上限应返回 413 而非 400。
func TestOversizedBody(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	// 合法的 JSON 但体积超过上限,触发 MaxBytesError。
	big := `{"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body head=%s", resp.StatusCode, body[:min(len(body), 120)])
	}
}

func TestMaskKey(t *testing.T) {
	if maskKey("") != "" {
		t.Error("maskKey(\"\") should be empty")
	}
	if maskKey("ab") != "****" {
		t.Errorf("maskKey(\"ab\") = %q", maskKey("ab"))
	}
	for _, k := range []string{"abcdefghi", "sk-abcde12", "sk-secret-1234", "sk-ant-very-long-key-value"} {
		m := maskKey(k)
		if m == k {
			t.Errorf("maskKey(%q) = itself", k)
		}
		if !strings.Contains(m, "****") {
			t.Errorf("maskKey(%q) = %q, want mask marker", k, m)
		}
		visible := len([]rune(m)) - 4
		if hidden := len([]rune(k)) - visible; hidden < 3 {
			t.Errorf("maskKey(%q) = %q hides only %d chars", k, m, hidden)
		}
	}
}
