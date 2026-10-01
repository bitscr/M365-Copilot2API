package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The console SPA is served from internal/web/web (//go:embed all:web) while
// web/ is the editable mirror of the same file. The two copies drifted apart
// once already, so pin them byte-for-byte: edit one, copy it to the other.
func TestWebIndexCopiesStayInSync(t *testing.T) {
	mirror, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mirror, embedded) {
		t.Fatal("web/index.html and internal/web/web/index.html differ — edit one and copy it to the other")
	}
}

func TestWebIndexIncludesBulkAccountDelete(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		`onclick="deleteSelectedAccounts()"`,
		`async function deleteSelectedAccounts()`,
		`JSON.stringify({ids,delete:true})`,
		`'Delete selected': {'zh-CN':'删除所选'}`,
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("web index missing bulk account delete wiring %q", needle)
		}
	}
}
