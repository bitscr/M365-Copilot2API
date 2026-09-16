package chathub

import (
	"strings"
)

// fileTypeFromMime maps a media type to the extension ChatHub expects in its
// fileType field. It returns "" when the type is unknown, so the caller can
// fall back to whatever the upload response reports.
func fileTypeFromMime(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "application/pdf":
		return "pdf"
	case "text/plain":
		return "txt"
	case "text/markdown":
		return "md"
	case "text/csv":
		return "csv"
	case "application/json":
		return "json"
	case "application/rtf", "text/rtf":
		return "rtf"
	case "application/msword":
		return "doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.ms-excel":
		return "xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.ms-powerpoint":
		return "ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	case "application/vnd.oasis.opendocument.text":
		return "odt"
	case "application/vnd.oasis.opendocument.spreadsheet":
		return "ods"
	case "application/zip":
		return "zip"
	}
	if strings.HasPrefix(m, "image/") {
		sub := strings.TrimPrefix(m, "image/")
		if sub == "jpeg" {
			return "jpg"
		}
		return sub
	}
	return ""
}
