package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupBackupEnv points every data-file env var into a fresh temp dir and
// seeds the six files with minimal valid content.
func setupBackupEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_CONFIG", filepath.Join(dir, "accounts.json"))
	t.Setenv("M365_API_KEYS", filepath.Join(dir, "api-keys.json"))
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_MASTER_KEY", "")
	seed := map[string]string{
		"accounts.json":        `{"accounts":[{"email":"a@x.com","refreshToken":"rt1"},{"email":"b@x.com","refreshToken":""}]}`,
		"api-keys.json":        `{"keys":[{"id":"k1","name":"default"}]}`,
		"admin-password.json":  `{"hash":"$2a$10$abcdefghijklmnopqrstuv"}`,
		"sessions.json":        `[{"id":"s1","contextHistory":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}]`,
		"active-sessions.json": `{"s1":{"id":"s1"}}`,
		"conversations.json":   `{"conversations":{"c1":{"id":"c1","title":"t"}}}`,
	}
	for name, content := range seed {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func doExport(t *testing.T, s *Server) ([]byte, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/export", nil)
	rec := httptest.NewRecorder()
	s.adminExportBackup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/zip" {
		t.Fatalf("export content-type = %q", ct)
	}
	return rec.Body.Bytes(), rec.Header().Get("Content-Disposition")
}

func zipNames(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[zf.Name] = b
	}
	return out
}

func postImport(t *testing.T, s *Server, zipData []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "m365-backup-test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipData); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.adminImportBackup(rec, req)
	return rec
}

func TestBackupExportRoundTrip(t *testing.T) {
	dir := setupBackupEnv(t)
	s := &Server{}
	zipData, disp := doExport(t, s)
	if !strings.Contains(disp, "m365-backup-") {
		t.Fatalf("content-disposition = %q", disp)
	}
	names := zipNames(t, zipData)
	for _, want := range []string{"accounts.json", "api-keys.json", "admin-password.json", "sessions.json", "active-sessions.json", "conversations.json", "manifest.json"} {
		if _, ok := names[want]; !ok {
			t.Errorf("zip missing %s (have %v)", want, keysOf(names))
		}
	}
	var mf backupManifest
	if err := json.Unmarshal(names["manifest.json"], &mf); err != nil {
		t.Fatalf("manifest unparsable: %v", err)
	}
	if mf.Version != backupManifestVersion || len(mf.Files) != 6 {
		t.Fatalf("manifest wrong: %+v", mf)
	}
	// 导出内容必须与种子数据一致。
	if !bytes.Contains(names["sessions.json"], []byte(`"contextHistory"`)) {
		t.Error("sessions content mismatch")
	}
	_ = dir
}

func TestBackupImportAppliesAndRotates(t *testing.T) {
	dir := setupBackupEnv(t)
	s := &Server{}
	orig := scheduleImportRestart
	scheduleImportRestart = func() {}
	defer func() { scheduleImportRestart = orig }()

	zipData, _ := doExport(t, s)
	rec := postImport(t, s, zipData)
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var sum backupImportSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Status != "imported" || sum.Accounts != 2 || sum.APIKeys != 1 || sum.Sessions != 1 || sum.Messages != 2 || sum.Conversations != 1 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	if len(sum.Warnings) == 0 {
		t.Error("expected a warning for the account missing refreshToken")
	}
	if len(sum.Restored) != 6 || len(sum.RotatedTo) != 6 {
		t.Fatalf("restored/rotated counts wrong: %+v / %+v", sum.Restored, sum.RotatedTo)
	}
	for _, rot := range sum.RotatedTo {
		if _, err := os.Stat(rot); err != nil {
			t.Errorf("rotated backup missing: %s (%v)", rot, err)
		}
		if !strings.HasPrefix(rot, dir+string(os.PathSeparator)) || !strings.Contains(rot, ".pre-import-") {
			t.Errorf("rotated path unexpected: %s", rot)
		}
	}
	// 磁盘上的文件必须仍是合法状态（导入内容与源相同，二次导出应成功）。
	zipData2, _ := doExport(t, s)
	names2 := zipNames(t, zipData2)
	for _, name := range []string{"accounts.json", "sessions.json", "api-keys.json"} {
		if _, ok := names2[name]; !ok {
			t.Errorf("post-import export missing %s", name)
		}
	}
}

func TestBackupImportRejectsBadShapes(t *testing.T) {
	dir := setupBackupEnv(t)
	s := &Server{}
	orig := scheduleImportRestart
	scheduleImportRestart = func() {}
	defer func() { scheduleImportRestart = orig }()

	before := readAllDataFiles(t, dir)

	// sessions.json 被替换成对象（应为数组）——整个导入必须被拒绝。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range backupFiles() {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		fw, _ := zw.Create(f.name)
		fw.Write(b)
	}
	fw, _ := zw.Create("sessions.json")
	fw.Write([]byte(`{"not":"an array"}`))
	mb, _ := json.Marshal(backupManifest{Version: backupManifestVersion, ExportedAt: "x", Files: []string{"sessions.json"}})
	mfw, _ := zw.Create("manifest.json")
	mfw.Write(mb)
	zw.Close()

	rec := postImport(t, s, buf.Bytes())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	after := readAllDataFiles(t, dir)
	if !bytes.Equal(before["sessions.json"], after["sessions.json"]) {
		t.Error("disk changed despite rejected import")
	}
	if glob, _ := filepath.Glob(filepath.Join(dir, "*.pre-import-*")); len(glob) != 0 {
		t.Errorf("rotation happened despite rejected import: %v", glob)
	}
}

func TestBackupImportRejectsZipSlip(t *testing.T) {
	dir := setupBackupEnv(t)
	s := &Server{}
	orig := scheduleImportRestart
	scheduleImportRestart = func() {}
	defer func() { scheduleImportRestart = orig }()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, _ := zw.Create("../evil")
	fw.Write([]byte("x"))
	mb, _ := json.Marshal(backupManifest{Version: backupManifestVersion, ExportedAt: "x"})
	mfw, _ := zw.Create("manifest.json")
	mfw.Write(mb)
	zw.Close()

	rec := postImport(t, s, buf.Bytes())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for zip-slip, got %d", rec.Code)
	}
	// 数据目录必须保持原样：只有 6 个种子文件，无旋转、无额外文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("data dir changed after rejected zip-slip import: %v", names)
	}
}

func TestBackupExportSkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_CONFIG", filepath.Join(dir, "accounts.json"))
	t.Setenv("M365_API_KEYS", filepath.Join(dir, "api-keys.json"))
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{"accounts":[]}`), 0o600)
	s := &Server{}
	zipData, _ := doExport(t, s)
	names := zipNames(t, zipData)
	if _, ok := names["sessions.json"]; ok {
		t.Error("missing source file should not be exported")
	}
	var mf backupManifest
	json.Unmarshal(names["manifest.json"], &mf)
	if len(mf.Notes) == 0 {
		t.Error("manifest should note skipped files")
	}
}

func readAllDataFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"accounts.json", "api-keys.json", "admin-password.json", "sessions.json", "active-sessions.json", "conversations.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
