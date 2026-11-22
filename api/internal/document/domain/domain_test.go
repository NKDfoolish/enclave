package domain

import (
	"archive/zip"
	"bytes"
	"testing"

	workspacedomain "enclave/api/internal/workspace/domain"
)

func TestVisibilityRules(t *testing.T) {
	internalReady := Document{Visibility: VisibilityInternal, Status: StatusReady}
	sharedReady := Document{Visibility: VisibilityClientCommon, Status: StatusReady}
	sharedProcessing := Document{Visibility: VisibilityClientCommon, Status: StatusProcessing}

	tests := []struct {
		name string
		doc  Document
		role workspacedomain.Role
		want bool
	}{
		{"admin sees internal", internalReady, workspacedomain.RoleAdmin, true},
		{"staff sees internal", internalReady, workspacedomain.RoleStaff, true},
		{"staff sees unfinished uploads", sharedProcessing, workspacedomain.RoleStaff, true},
		{"client does not see internal", internalReady, workspacedomain.RoleClient, false},
		{"client sees shared ready", sharedReady, workspacedomain.RoleClient, true},
		{"client does not see shared but unchecked", sharedProcessing, workspacedomain.RoleClient, false},
	}
	for _, tc := range tests {
		if got := tc.doc.IsVisibleTo(tc.role); got != tc.want {
			t.Errorf("%s: IsVisibleTo = %v, want %v", tc.name, got, tc.want)
		}
	}

	if !CanManage(workspacedomain.RoleStaff) || CanManage(workspacedomain.RoleClient) {
		t.Error("staff manage documents, clients do not")
	}
}

func TestCleanFilename(t *testing.T) {
	tests := []struct {
		input  string
		want   string
		wantOK bool
	}{
		{"report.pdf", "report.pdf", true},
		{"  Quý 3 báo cáo.docx ", "Quý 3 báo cáo.docx", true},
		{"../../etc/passwd", "passwd", true},
		{`C:\Users\x\notes.md`, "notes.md", true},
		{"", "", false},
		{"bad\nname.pdf", "", false},
		{"/", "", false},
	}
	for _, tc := range tests {
		got, ok := CleanFilename(tc.input)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("CleanFilename(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestContentTypeFromFilename(t *testing.T) {
	tests := map[string]ContentType{"a.pdf": ContentTypePDF, "A.PDF": ContentTypePDF, "b.md": ContentTypeMarkdown, "c.docx": ContentTypeDOCX}
	for name, want := range tests {
		if got, ok := ContentTypeFromFilename(name); !ok || got != want {
			t.Errorf("ContentTypeFromFilename(%q) = %q, %v", name, got, ok)
		}
	}
	for _, name := range []string{"x.exe", "x.html", "x.svg", "noext", "x.pdf.exe"} {
		if _, ok := ContentTypeFromFilename(name); ok {
			t.Errorf("%q must be rejected", name)
		}
	}
}

func TestValidateContent(t *testing.T) {
	docx := zipWith(t, "word/document.xml")
	notDocx := zipWith(t, "readme.txt")

	tests := []struct {
		name        string
		contentType ContentType
		data        []byte
		wantErr     bool
	}{
		{"real PDF", ContentTypePDF, []byte("%PDF-1.7\n..."), false},
		{"executable named .pdf", ContentTypePDF, []byte("MZ\x90\x00 this is an exe"), true},
		{"markdown", ContentTypeMarkdown, []byte("# Policy\n\nTiếng Việt ok."), false},
		{"binary named .md", ContentTypeMarkdown, []byte("text\x00binary"), true},
		{"invalid UTF-8 named .md", ContentTypeMarkdown, []byte{0xff, 0xfe, 0x41}, true},
		{"real DOCX", ContentTypeDOCX, docx, false},
		{"zip that is not a DOCX", ContentTypeDOCX, notDocx, true},
		{"PDF named .docx", ContentTypeDOCX, []byte("%PDF-1.7"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateContent(tc.contentType, tc.data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func zipWith(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("<xml/>"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
