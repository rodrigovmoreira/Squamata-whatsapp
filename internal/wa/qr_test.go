package wa

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestQRDataURIIsAPNG(t *testing.T) {
	dataURI, err := qrDataURI("2@abc123,def456,ghi789")
	if err != nil {
		t.Fatalf("qrDataURI: %v", err)
	}
	if !strings.HasPrefix(dataURI, qrImagePrefix) {
		t.Fatalf("prefixo = %q, esperado %q", dataURI[:30], qrImagePrefix)
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURI, qrImagePrefix))
	if err != nil {
		t.Fatalf("base64 inválido: %v", err)
	}
	if len(raw) < 8 {
		t.Fatalf("imagem pequena demais: %d bytes", len(raw))
	}
	// Assinatura PNG: 89 50 4E 47 0D 0A 1A 0A
	pngMagic := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	for i, b := range pngMagic {
		if raw[i] != b {
			t.Fatalf("byte %d = 0x%02X, esperado 0x%02X (não é PNG)", i, raw[i], b)
		}
	}
}
