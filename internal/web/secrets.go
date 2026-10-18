package web

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net/http"
	"strings"

	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
)

// generatedSecret is a value the deploy form made up for a required secret left empty.
type generatedSecret struct{ Label, Value string }

// generateSecrets fills the required secret fields left empty in cfg with random values.
func generateSecrets(schema []byte, cfg map[string]any) ([]generatedSecret, error) {
	fields, err := schemaform.Build(schema, cfgPrefix, nil)
	if err != nil {
		return nil, err
	}
	var out []generatedSecret
	for _, f := range fields {
		key := strings.TrimPrefix(f.Path, "/")
		if _, set := cfg[key]; set || f.Kind != schemaform.Secret || !f.Required {
			continue
		}
		v, err := newSecretValue(f.Pattern)
		if err != nil {
			return nil, err
		}
		cfg[key] = v
		out = append(out, generatedSecret{Label: f.Label, Value: v})
	}
	return out, nil
}

const secretAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// newSecretValue is 64 hex digits where the schema asks for hex (S3 keys), else 24
// letters and digits that are easy to read back.
func newSecretValue(pattern string) (string, error) {
	if strings.Contains(pattern, "[0-9a-f]") {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
	out := make([]byte, 24)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(secretAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = secretAlphabet[n.Int64()]
	}
	return string(out), nil
}

// storedSecrets names the secret fields b already holds a value for.
func storedSecrets(b *pb.Block) map[string]bool {
	out := map[string]bool{}
	for _, r := range b.GetSpec().GetSecrets() {
		out[r.GetPath()] = true
	}
	return out
}

// markSecrets says what an empty secret input does: keeps the stored value on edit,
// or makes one up on deploy.
func markSecrets(fields []schemaform.Field, stored map[string]bool, edit bool) {
	for i := range fields {
		f := &fields[i]
		if f.Kind != schemaform.Secret {
			continue
		}
		switch key := strings.TrimPrefix(f.Path, "/"); {
		case edit && stored[key]:
			f.Required = false
			f.Help = joinHint(f.Help, "A value is stored sealed. Leave empty to keep it.")
		case !edit && f.Required:
			f.Required = false
			f.Help = joinHint(f.Help, "Leave empty to generate one; it is shown once after Deploy.")
		}
	}
}

func joinHint(help, hint string) string {
	if help == "" {
		return hint
	}
	return strings.TrimSuffix(help, ".") + ". " + hint
}

type deploySecretsData struct {
	Page    page
	Block   string // namespace/name
	Secrets []generatedSecret
}

// renderGenerated shows the generated secrets once, in a page no cache keeps.
func (s *Server) renderGenerated(w http.ResponseWriter, r *http.Request, full string, gen []generatedSecret) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "deploy_secrets.html", deploySecretsData{
		Page: s.newPage(w, r, "Deployed "+full, "blocks",
			crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: full, Href: "/blocks/" + full}),
		Block: full, Secrets: gen,
	})
}
