package aiengine_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/BlackVS/aiengine"
	"github.com/BlackVS/aiengine/cli"
	"github.com/BlackVS/aiengine/openai"
	"github.com/BlackVS/aiengine/pace"
	"github.com/BlackVS/aiengine/provider"
)

// The README's examples, compiled so they cannot drift from the API.
// They have no Output comment, so go test builds them but never runs
// them: they would call real endpoints.

func Example_apiKeyProvider() {
	ctx := context.Background()
	reg := provider.Load("/path/to/providers.json")
	ep, why := reg.Lookup("gpt4o", provider.KindOpenAI, provider.Fallback{})
	if why != "" {
		fmt.Println(why)
		return
	}
	pacer := pace.New(pace.Options{Interval: pace.DefaultInterval, Retries: pace.DefaultRetries})
	c := &openai.Client{BaseURL: ep.BaseURL, Token: ep.Token, Pacer: pacer}
	answer, usage, err := c.Complete(ctx, ep.Model, "Summarise this in one line: ...")
	fmt.Println(answer, usage, err)

	e := &openai.Client{BaseURL: ep.BaseURL, Token: ep.Token, Dimensions: 768, Pacer: pacer}
	vectors, usage, err := e.Embed(ctx, ep.Model, []string{"first text", "second text"})
	fmt.Println(len(vectors), usage, err)
}

func Example_fallback() {
	cfg := struct{ BaseURL, APIKey string }{"https://llm.example/v1", "sk-..."}
	fb := provider.Fallback{BaseURL: cfg.BaseURL, Token: cfg.APIKey, Source: "the API key setting"}
	ep, ok := provider.Resolve("/path/to/providers.json", "gpt-4o-mini", fb)
	fmt.Println(ep.BaseURL, ok)
}

func Example_cliProvider() {
	ctx := context.Background()
	reg := provider.Load("/path/to/providers.json")
	pacer := pace.New(pace.Options{Interval: pace.DefaultInterval})
	scratchDir, prompt := "/tmp/scratch", "..."
	ep, isBound, why := reg.ResolveBound("haiku")
	if isBound && why == "" && ep.Kind == provider.KindClaude {
		r := &cli.Runner{Vendor: cli.Claude{}, WorkDir: scratchDir, Pacer: pacer}
		answer, usage, err := r.Complete(ctx, ep.Model, prompt)
		if errors.Is(err, aiengine.ErrLoginExpired) {
			fmt.Println("sign in again in the claude CLI")
		}
		fmt.Println(answer, usage)

		res := aiengine.Probe(ctx, r, "haiku")
		fmt.Println(res.Auth, res.Version, res.Answer)
	}
}
