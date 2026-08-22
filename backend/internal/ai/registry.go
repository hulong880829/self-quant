package ai

import (
	"fmt"
	"strings"
)

type ProviderRegistry struct {
	providers map[string]ModelProvider
}

func NewProviderRegistry(providers ...ModelProvider) *ProviderRegistry {
	registry := &ProviderRegistry{providers: make(map[string]ModelProvider, len(providers))}
	for _, provider := range providers {
		if provider != nil {
			registry.providers[strings.ToLower(strings.TrimSpace(provider.Name()))] = provider
		}
	}
	return registry
}

func (r *ProviderRegistry) Get(name string) (ModelProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: provider registry is unavailable", ErrProviderUnavailable)
	}
	provider, ok := r.providers[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported provider", ErrInvalidRequest)
	}
	return provider, nil
}
