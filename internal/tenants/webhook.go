package tenants

import (
	"context"
	"time"
)

// webhookCacheTTL evita uma consulta ao banco por evento publicado.
const webhookCacheTTL = 60 * time.Second

type cachedWebhook struct {
	url       string
	secret    string
	expiresAt time.Time
}

// Webhook resolve o destino dos eventos do tenant. Satisfaz a interface
// events.WebhookResolver sem que o pacote events precise conhecer tenants.
func (r *Repo) Webhook(ctx context.Context, tenantID string) (string, string) {
	r.cacheMu.Lock()
	if cached, ok := r.cache[tenantID]; ok && time.Now().Before(cached.expiresAt) {
		r.cacheMu.Unlock()
		return cached.url, cached.secret
	}
	r.cacheMu.Unlock()

	tenant, err := r.Get(ctx, tenantID)
	if err != nil {
		// Sem tenant registrado: quem chamou usa o webhook global (fallback).
		r.storeCache(tenantID, "", "")
		return "", ""
	}
	r.storeCache(tenantID, tenant.WebhookURL, tenant.WebhookSecret)
	return tenant.WebhookURL, tenant.WebhookSecret
}

func (r *Repo) storeCache(tenantID, url, secret string) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.cache == nil {
		r.cache = make(map[string]cachedWebhook)
	}
	r.cache[tenantID] = cachedWebhook{url: url, secret: secret, expiresAt: time.Now().Add(webhookCacheTTL)}
}

// InvalidateWebhook limpa o cache quando o webhook de um tenant muda.
func (r *Repo) InvalidateWebhook(tenantID string) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	delete(r.cache, tenantID)
}
