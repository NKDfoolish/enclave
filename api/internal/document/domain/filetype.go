package domain

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ContentType is one of the file types the library accepts.
type ContentType string

const (
	ContentTypePDF      ContentType = "application/pdf"
	ContentTypeMarkdown ContentType = "text/markdown"
	ContentTypeDOCX     ContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// ContentTypeFromFilename picks the type from the extension. The extension only decides
// what we expect; ValidateContent later checks that the bytes really are that type.
func ContentTypeFromFilename(filename string) (ContentType, bool) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".pdf":
		return ContentTypePDF, true
	case ".md", ".markdown":
		return ContentTypeMarkdown, true
	case ".docx":
		return ContentTypeDOCX, true
	default:
		return "", false
	}
}

// ValidateContent checks that data really is a file of type contentType, by looking at the
// bytes rather than trusting the name or the browser. It does not parse the whole document;
// Part 4's ingestion does that, with its own limits.
func ValidateContent(contentType ContentType, data []byte) error {
	switch contentType {
	case ContentTypePDF:
		return validatePDF(data)
	case ContentTypeMarkdown:
		return validateMarkdown(data)
	case ContentTypeDOCX:
		return validateDOCX(data)
	default:
		return fmt.Errorf("unknown content type %q", contentType)
	}
}

// A PDF starts with "%PDF-" within its first 1024 bytes (ISO 32000).
func validatePDF(data []byte) error {
	header := data[:min(len(data), 1024)]
	if !bytes.Contains(header, []byte("%PDF-")) {
		return errors.New("not a PDF: missing %PDF- header")
	}
	return nil
}

// Markdown is plain text: valid UTF-8 and no NUL bytes, which only binary files contain.
func validateMarkdown(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("not text: invalid UTF-8")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return errors.New("not text: contains NUL bytes")
	}
	return nil
}

// A DOCX is a ZIP archive containing word/document.xml. Only the archive's directory is read;
// nothing is decompressed, so a "zip bomb" cannot exhaust memory here.
func validateDOCX(data []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a DOCX: %w", err)
	}
	for _, file := range archive.File {
		if file.Name == "word/document.xml" {
			return nil
		}
	}
	return errors.New("not a DOCX: word/document.xml missing")
}
