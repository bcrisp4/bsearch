package main

import (
	"fmt"

	"github.com/bcrisp4/bsearch/internal/adapters/openai"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/embedding"
)

// loadInference loads config and builds the embedding client — the setup
// shared by index, search, and eval. All commands must resolve the exact
// same embedding spec from config, or queries would land in a different
// vector space than the index (DESIGN.md: prefix templates); sharing the
// wiring makes divergence impossible. The resolved spec is available as
// embedder.Spec() (returned verbatim, never normalized). The default-db-path
// check belongs to callers that own a db path (index, search); eval computes
// its own work-db path after the spec is known, so it has none to check.
func loadInference(configPath string) (*config.Config, *openai.Embedder, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Inference.EmbeddingModel == "" {
		return nil, nil, fmt.Errorf("inference.embedding_model is not set — add it to %s (the M2 bake-off records recommended defaults in DESIGN.md)", configPath)
	}
	spec := embedding.ResolveSpec(
		cfg.Inference.EmbeddingModel,
		cfg.Inference.QueryTemplate,
		cfg.Inference.PassageTemplate,
		cfg.Inference.InputCeilingTokens,
	)
	embedder, err := openai.NewEmbedder(openai.EmbedderConfig{
		Endpoint: cfg.Inference.Endpoint,
		Spec:     spec,
	})
	if err != nil {
		return nil, nil, err
	}
	return cfg, embedder, nil
}
