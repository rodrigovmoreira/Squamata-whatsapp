package config

import (
	"errors"
	"os"
	"strings"
)

// Config reune as variaveis de ambiente do servico.
type Config struct {
	Port          string
	DBPath        string
	InternalToken string
	// WebhookURL e WebhookSecret publicam os eventos de sessao para o
	// Calango-bot. Sem URL, o dispatcher fica desligado (dev sem o bot).
	WebhookURL    string
	WebhookSecret string
}

// Load le o ambiente e falha rapido quando falta o que nao pode faltar.
// O token interno e obrigatorio: subir sem ele seria aceitar qualquer chamada
// entre servicos (ver "Errar Rapido" no constitution.md).
func Load() (Config, error) {
	token := strings.TrimSpace(os.Getenv("SQUAMATA_INTERNAL_TOKEN"))
	if token == "" {
		return Config{}, errors.New("SQUAMATA_INTERNAL_TOKEN nao definido")
	}

	return Config{
		Port:          envOr("SQUAMATA_WHATSAPP_PORT", "3006"),
		DBPath:        envOr("SQUAMATA_WHATSAPP_DB_PATH", "./data/whatsapp.db"),
		InternalToken: token,
		WebhookURL:    envOr("SQUAMATA_WEBHOOK_URL", ""),
		WebhookSecret: envOr("SQUAMATA_WEBHOOK_SECRET", ""),
	}, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
