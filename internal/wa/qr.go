package wa

import (
	"encoding/base64"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	qrImageSize = 320
	// qrImagePrefix é o formato que o painel consome direto em <img src>.
	qrImagePrefix = "data:image/png;base64,"
)

// qrDataURI desenha o QR no próprio serviço (decisão D3): o painel só exibe,
// não precisa de biblioteca de QR. O payload continua sendo a credencial que
// o whatsmeow emite — a imagem é apenas a representação.
func qrDataURI(payload string) (string, error) {
	png, err := qrcode.Encode(payload, qrcode.Medium, qrImageSize)
	if err != nil {
		return "", fmt.Errorf("gerar imagem do QR: %w", err)
	}
	return qrImagePrefix + base64.StdEncoding.EncodeToString(png), nil
}
