package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
)

// 备份/还原功能：全量 zip 快照。
// 导出：accounts + api-keys + admin-password + sessions + active-sessions +
// conversations + manifest，逐个按所属 store 的路径解析规则定位磁盘文件。
// 导入：校验 zip 结构 -> 校验每个 json 的格式 -> 现场旋转备份 -> 原子写入
// -> 汇总报告 -> 自动重启生效。

const backupManifestVersion = 1

// backupFile 描述导出 zip 中的一个条目及其运行时磁盘路径。
type backupFile struct {
	name string // zip 内条目名（固定，无路径分隔符）
	path string // 磁盘路径，与所属 store 同规则解析
}

func usageFilePath() string {
	if p := strings.TrimSpace(os.Getenv("M365_USAGE_LOG")); p != "" {
		return p
	}
	dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, ".config", "m365-copilot2api")
	}
	return filepath.Join(dir, "usage.jsonl")
}

func statsFilePath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "stats.json")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "m365-copilot2api", "stats.json")
}

func backupFiles() []backupFile {
	home, _ := os.UserHomeDir()
	confDir := filepath.Join(home, ".config", "m365-copilot2api")
	resolve := func(env, def string) string {
		if p := strings.TrimSpace(os.Getenv(env)); p != "" {
			return p
		}
		return def
	}
	return []backupFile{
		{"accounts.json", auth.CachePath()},
		{"api-keys.json", resolve("M365_API_KEYS", filepath.Join(confDir, "api-keys.json"))},
		{"admin-password.json", adminPasswordPath()},
		{"sessions.json", resolve("M365_SESSION_CACHE", filepath.Join(confDir, "sessions.json"))},
		{"active-sessions.json", activeSessionCachePath()},
		{"conversations.json", resolve("M365_CONVERSATION_CACHE", filepath.Join(confDir, "conversations.json"))},
		{"usage.jsonl", usageFilePath()},
		{"stats.json", statsFilePath()},
	}
}

type backupManifest struct {
	Version      int      `json:"version"`
	ExportedAt   string   `json:"exported_at"`
	Files        []string `json:"files"`
	MasterKeySet bool     `json:"master_key_set"`
	Notes        []string `json:"notes,omitempty"`
}

// adminExportBackup 打包全量数据为 zip 下载。
func (s *Server) adminExportBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mf := backupManifest{
		Version:      backupManifestVersion,
		ExportedAt:   time.Now().UTC().Format(time.RFC3339),
		MasterKeySet: strings.TrimSpace(os.Getenv("M365_MASTER_KEY")) != "",
	}
	writeEntry := func(name string, b []byte) error {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(0o600)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = fw.Write(b)
		return err
	}
	for _, f := range backupFiles() {
		b, err := os.ReadFile(f.path)
		if err != nil {
			if os.IsNotExist(err) {
				mf.Notes = append(mf.Notes, "skipped "+f.name+": not present")
				continue
			}
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "read "+f.name+": "+err.Error())
			return
		}
		if err := writeEntry(f.name, b); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		mf.Files = append(mf.Files, f.name)
	}
	mb, _ := json.MarshalIndent(mf, "", "  ")
	if err := writeEntry("manifest.json", mb); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if err := zw.Close(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	ts := time.Now().Format("20060102-150405")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="m365-backup-%s.zip"`, ts))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

type backupImportSummary struct {
	Status        string   `json:"status"`
	Restored      []string `json:"restored"`
	RotatedTo     []string `json:"rotated_to"`
	Accounts      int      `json:"accounts"`
	APIKeys       int      `json:"api_keys"`
	Sessions      int      `json:"sessions"`
	Messages      int      `json:"messages"`
	Conversations int      `json:"conversations"`
	UsageRecords  int      `json:"usage_records"`
	Restart       string   `json:"restart"`
	Warnings      []string `json:"warnings"`
}

// adminImportBackup 校验并应用一个备份 zip。失败时不做任何磁盘变更。
func (s *Server) adminImportBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "expected multipart form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing file field")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 512<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "read upload: "+err.Error())
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "not a valid zip: "+err.Error())
		return
	}

	known := map[string]backupFile{}
	for _, f := range backupFiles() {
		known[f.name] = f
	}
	entries := map[string][]byte{} // 条目名 -> 内容（全部通过校验后才落盘）
	var manifest *backupManifest
	for _, zf := range zr.File {
		name := zf.Name
		// 禁止路径穿越与子目录：条目名必须是无路径分隔符的已知平铺文件名。
		if name == "" || name == "." || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, "..") {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "suspicious zip entry name: "+name)
			return
		}
		if zf.FileInfo().IsDir() {
			continue
		}
		if name == "manifest.json" {
			rc, err := zf.Open()
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "open manifest: "+err.Error())
				return
			}
			mb, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil || json.Unmarshal(mb, &manifest) != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "manifest.json is not valid JSON")
				return
			}
			continue
		}
		bf, ok := known[name]
		if !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unexpected file in backup: "+name)
			return
		}
		rc, err := zf.Open()
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "open "+name+": "+err.Error())
			return
		}
		content, err := io.ReadAll(io.LimitReader(rc, 128<<20))
		rc.Close()
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "read "+name+": "+err.Error())
			return
		}
		if err := validateBackupJSON(name, content); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		entries[bf.name] = content
	}
	if manifest == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "zip must contain manifest.json")
		return
	}
	if manifest.Version != backupManifestVersion {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("unsupported backup version %d (expected %d)", manifest.Version, backupManifestVersion))
		return
	}

	// 全部校验通过，开始应用：现场旋转 -> 原子写入；任一步失败则回滚。
	ts := time.Now().Format("20060102-150405")
	files := backupFiles()
	var rotated []string // 与 files 下标对齐，记录该文件是否被改名保留
	rotatedPath := make([]string, len(files))
	for i, f := range files {
		if _, ok := entries[f.name]; !ok {
			continue
		}
		if _, err := os.Stat(f.path); err == nil {
			rot := f.path + ".pre-import-" + ts
			if err := os.Rename(f.path, rot); err != nil {
				rollbackImport(files, rotatedPath)
				writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "rotate "+f.name+": "+err.Error())
				return
			}
			rotatedPath[i] = rot
			rotated = append(rotated, rot)
		}
	}
	for _, f := range files {
		content, ok := entries[f.name]
		if !ok {
			continue
		}
		if err := writeFileAtomic(f.path, content, 0o600); err != nil {
			rollbackImport(files, rotatedPath)
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "write "+f.name+": "+err.Error())
			return
		}
	}

	summary := backupImportSummary{
		Status:        "imported",
		Restart:       RestartStrategy(),
		Conversations: countBackupJSON("conversations.json", entries["conversations.json"]),
	}
	for _, f := range files {
		if c, ok := entries[f.name]; ok {
			summary.Restored = append(summary.Restored, f.name)
			switch f.name {
			case "accounts.json":
				summary.Accounts = countBackupJSON(f.name, c)
			case "api-keys.json":
				summary.APIKeys = countBackupJSON(f.name, c)
			case "sessions.json":
				summary.Sessions = countBackupJSON(f.name, c)
				summary.Messages = countBackupMessages(c)
			case "usage.jsonl":
				summary.UsageRecords = countBackupJSON(f.name, c)
			}
		}
	}
	summary.RotatedTo = rotated
	summary.Warnings = importWarnings(manifest, entries["accounts.json"])
	log.Printf("[backup] import applied: %s (accounts=%d keys=%d sessions=%d msgs=%d conversations=%d usage=%d)",
		strings.Join(summary.Restored, ","), summary.Accounts, summary.APIKeys, summary.Sessions, summary.Messages, summary.Conversations, summary.UsageRecords)
	jsonOut(w, summary)
	scheduleImportRestart()
}

// scheduleImportRestart indirection keeps the import path testable: the real
// scheduleRestart would re-exec the test binary in its "exec" branch.
var scheduleImportRestart = scheduleRestart

// rollbackImport 撤销已旋转/已写入的文件，恢复导入前状态（尽力而为）。
func rollbackImport(files []backupFile, rotatedPath []string) {
	for i := range files {
		p := files[i].path
		_ = os.Remove(p)
		if rotatedPath[i] != "" {
			_ = os.Rename(rotatedPath[i], p)
		}
	}
	log.Printf("[backup] import rolled back after failure")
}

func validateBackupJSON(name string, b []byte) error {
	switch name {
	case "accounts.json":
		var a struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(b, &a); err == nil && a.Accounts != nil {
			return nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(b, &arr); err == nil {
			return nil
		}
		return fmt.Errorf("accounts.json must be an object with \"accounts\" or a JSON array")
	case "api-keys.json":
		var k struct {
			Keys []json.RawMessage `json:"keys"`
		}
		if err := json.Unmarshal(b, &k); err != nil || k.Keys == nil {
			return fmt.Errorf("api-keys.json must be a JSON object with a \"keys\" array")
		}
	case "sessions.json":
		var arr []json.RawMessage
		if err := json.Unmarshal(b, &arr); err != nil {
			return fmt.Errorf("sessions.json must be a JSON array")
		}
	case "active-sessions.json":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return fmt.Errorf("active-sessions.json must be a JSON object")
		}
	case "conversations.json":
		var c struct {
			Conversations map[string]json.RawMessage `json:"conversations"`
		}
		if err := json.Unmarshal(b, &c); err != nil || c.Conversations == nil {
			return fmt.Errorf("conversations.json must be a JSON object with a \"conversations\" object")
		}
	case "admin-password.json":
		if !json.Valid(b) {
			return fmt.Errorf("admin-password.json is not valid JSON")
		}
	case "usage.jsonl":
		// 每行一个 JSON 对象。
		for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			l := strings.TrimSpace(line)
			if l == "" {
				continue
			}
			var rec json.RawMessage
			if !json.Valid([]byte(l)) {
				return fmt.Errorf("usage.jsonl line %d is not valid JSON", i+1)
			}
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				return fmt.Errorf("usage.jsonl line %d: %v", i+1, err)
			}
		}
	case "stats.json":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return fmt.Errorf("stats.json must be a JSON object")
		}
	}
	return nil
}

func countBackupJSON(name string, b []byte) int {
	if len(b) == 0 {
		return 0
	}
	switch name {
	case "accounts.json":
		var a struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if json.Unmarshal(b, &a) == nil && a.Accounts != nil {
			return len(a.Accounts)
		}
		var arr []json.RawMessage
		if json.Unmarshal(b, &arr) == nil {
			return len(arr)
		}
	case "api-keys.json":
		var k struct {
			Keys []json.RawMessage `json:"keys"`
		}
		if json.Unmarshal(b, &k) == nil {
			return len(k.Keys)
		}
	case "sessions.json":
		var arr []json.RawMessage
		if json.Unmarshal(b, &arr) == nil {
			return len(arr)
		}
	case "conversations.json":
		var c struct {
			Conversations map[string]json.RawMessage `json:"conversations"`
		}
		if json.Unmarshal(b, &c) == nil {
			return len(c.Conversations)
		}
	case "usage.jsonl":
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		return n
	}
	return 0
}

func countBackupMessages(b []byte) int {
	var arr []struct {
		ContextHistory []json.RawMessage `json:"contextHistory"`
	}
	if json.Unmarshal(b, &arr) != nil {
		return 0
	}
	n := 0
	for _, s := range arr {
		n += len(s.ContextHistory)
	}
	return n
}

// importWarnings 生成导入后的风险提示：主密钥不匹配、需要重新登录的账号。
func importWarnings(m *backupManifest, accounts []byte) []string {
	var warns []string
	curKey := strings.TrimSpace(os.Getenv("M365_MASTER_KEY")) != ""
	if m.MasterKeySet && !curKey {
		warns = append(warns, "备份导出时设置了 M365_MASTER_KEY，当前实例未设置：账号 refresh token 可能无法解密")
	} else if !m.MasterKeySet && curKey {
		warns = append(warns, "备份导出时未设置 M365_MASTER_KEY（内置 fallback 密钥），当前实例已设置：账号 refresh token 可能无法解密")
	}
	var a struct {
		Accounts []struct {
			Email        string `json:"email"`
			RefreshToken string `json:"refreshToken"`
		} `json:"accounts"`
	}
	if json.Unmarshal(accounts, &a) != nil {
		var arr []struct {
			Email        string `json:"email"`
			RefreshToken string `json:"refreshToken"`
		}
		_ = json.Unmarshal(accounts, &arr)
		a.Accounts = arr
	}
	needLogin := 0
	exemplar := ""
	for _, acc := range a.Accounts {
		if strings.TrimSpace(acc.RefreshToken) == "" {
			needLogin++
			if exemplar == "" {
				exemplar = acc.Email
			}
		}
	}
	if needLogin > 0 {
		warns = append(warns, fmt.Sprintf("%d 个账号缺少 refresh token（如 %s），需重新登录", needLogin, exemplar))
	}
	return warns
}
