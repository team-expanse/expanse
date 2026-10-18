// Deploy pages (PHASE-14 stream D): pick a catalog type, fill in a form built from its schema
// and template, and create the block. The YAML page (/blocks/new) stays for manifests.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/blocks/apply"
	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"
)

const cfgPrefix = "cfg"

// blockChecker is the block service's full admission check (service.Server.Check).
type blockChecker interface {
	Check(ctx context.Context, b *pb.Block, update bool) []validate.ValidationError
}

func (s *Server) registerDeployRoutes() {
	s.routes.Handle("GET /deploy", s.requireAuth(http.HandlerFunc(s.handleDeployCatalog)))
	s.routes.Handle("GET /deploy/{cat}/{name}", s.requireAuth(http.HandlerFunc(s.handleDeployForm)))
	s.routes.Handle("POST /deploy/{cat}/{name}", s.requireAuth(http.HandlerFunc(s.handleDeploySubmit)))
	s.routes.Handle("GET /blocks/{ns}/{name}/edit", s.requireAuth(http.HandlerFunc(s.handleEditForm)))
	s.routes.Handle("POST /blocks/{ns}/{name}/edit", s.requireAuth(http.HandlerFunc(s.handleEditSubmit)))
}

// clusterFacts is what the deploy pages know about the nodes; zero nodes means unknown.
type clusterFacts struct {
	nodes int
	caps  map[string]bool
}

func (s *Server) loadClusterFacts(ctx context.Context) clusterFacts {
	f := clusterFacts{caps: map[string]bool{}}
	entries, err := s.store.List(store.WithStale(ctx), "/nodes/")
	if err != nil {
		return f
	}
	for _, e := range entries {
		rest := strings.TrimPrefix(string(e.Key), "/nodes/")
		switch {
		case rest != "" && !strings.Contains(rest, "/"):
			f.nodes++
		case strings.HasSuffix(rest, "/capabilities"):
			for _, c := range strings.Split(strings.TrimSpace(string(e.Value)), ",") {
				if c != "" {
					f.caps[c] = true
				}
			}
		}
	}
	return f
}

// missing lists the capabilities no node has; nothing when the nodes are unknown.
func (f clusterFacts) missing(need []string) []string {
	if f.nodes == 0 {
		return nil
	}
	var out []string
	for _, c := range need {
		if !f.caps[c] {
			out = append(out, c)
		}
	}
	return out
}

type deployTypeView struct {
	ID, Description string
	Needs, Missing  []string
}

type deployCatalogData struct {
	Page  page
	Types []deployTypeView
}

func (s *Server) handleDeployCatalog(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) || s.catalogUnavailable(w, r) {
		return
	}
	resp, err := s.catalog.ListTypes(r.Context(), &pb.ListTypesRequest{})
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return
	}
	facts := s.loadClusterFacts(r.Context())
	var views []deployTypeView
	for _, bt := range resp.GetTypes() {
		typ, err := catalogType(bt)
		if err != nil {
			continue
		}
		b, err := apply.FromTemplate(typ, "", "x", nil)
		if err != nil {
			continue
		}
		need := b.GetSpec().GetPlacement().GetRequiredCapabilities()
		views = append(views, deployTypeView{ID: bt.GetName(), Description: bt.GetDescription(), Needs: need, Missing: facts.missing(need)})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	s.render(w, http.StatusOK, "deploy_catalog.html", deployCatalogData{
		Page:  s.newPage(w, r, "Deploy a block", "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: "Deploy"}),
		Types: views,
	})
}

func (s *Server) catalogUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.catalog != nil {
		return false
	}
	s.renderError(w, r, http.StatusServiceUnavailable, "The block catalog is not available on this node.")
	return true
}

// catalogType rebuilds the catalog entry FromTemplate needs from the API's view of it.
func catalogType(bt *pb.BlockType) (*catalog.Type, error) {
	cat, name, _ := strings.Cut(bt.GetName(), "/")
	typ := &catalog.Type{Category: cat, Name: name, Version: bt.GetVersion(), SchemaJSON: bt.GetSchemaJson()}
	if len(bt.GetDefaultsJson()) > 0 {
		if err := json.Unmarshal(bt.GetDefaultsJson(), &typ.Defaults); err != nil {
			return nil, err
		}
	}
	if len(bt.GetTemplateJson()) > 0 {
		if err := json.Unmarshal(bt.GetTemplateJson(), &typ.Template); err != nil {
			return nil, err
		}
	}
	return typ, nil
}

type storageInput struct {
	Name, Size  string
	Replication int32
}

type deployFormData struct {
	Page         page
	Type         *pb.BlockType
	Action       string
	Edit         bool   // editing an existing block: the name is fixed
	Cancel       string // where Cancel goes
	Name         string
	NameError    string
	Replicas     string
	ShowReplicas bool
	CPU, Memory  string
	Storage      []storageInput
	Ports        []*pb.Port
	Fields       []schemaform.Field
	Errors       []string
	Manifest     string
}

func (s *Server) deployType(w http.ResponseWriter, r *http.Request) (*pb.BlockType, *catalog.Type, bool) {
	if s.blocksUnavailable(w, r) || s.catalogUnavailable(w, r) {
		return nil, nil, false
	}
	bt, err := s.catalog.GetType(r.Context(), &pb.GetTypeRequest{Name: r.PathValue("cat") + "/" + r.PathValue("name")})
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return nil, nil, false
	}
	typ, err := catalogType(bt)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return nil, nil, false
	}
	return bt, typ, true
}

func (s *Server) handleDeployForm(w http.ResponseWriter, r *http.Request) {
	bt, typ, ok := s.deployType(w, r)
	if !ok {
		return
	}
	b, err := apply.FromTemplate(typ, "default", typ.Name, nil)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	fitToCluster(b, s.loadClusterFacts(r.Context()).nodes)
	fields, err := schemaform.Build(bt.GetSchemaJson(), cfgPrefix, typ.Defaults)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	data := s.deployData(w, r, bt, b, fields)
	s.render(w, http.StatusOK, "deploy_form.html", data)
}

func (s *Server) handleDeploySubmit(w http.ResponseWriter, r *http.Request) {
	bt, typ, ok := s.deployType(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	cfg, cfgErrs := schemaform.Decode(bt.GetSchemaJson(), cfgPrefix, form)
	b, err := apply.FromTemplate(typ, "default", strings.TrimSpace(form.Get("name")), cfg)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	fitToCluster(b, s.loadClusterFacts(r.Context()).nodes)
	followPorts(b, typ.Defaults)
	sub := formErrors{config: cfgErrs, general: applyCommon(b, form)}
	if sub.admit(r.Context(), s.blocks, b, false) {
		created, err := s.blocks.Create(r.Context(), b)
		if err == nil {
			ns, name := created.GetMetadata().GetNamespace(), created.GetMetadata().GetName()
			s.done(w, r, "success", "Block "+ns+"/"+name+" deployed", "/blocks/"+ns+"/"+name)
			return
		}
		sub.general = append(sub.general, errText(err))
	}
	s.renderRejected(w, r, s.deployData(w, r, bt, b, nil), form, sub)
}

func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	b, bt, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	fields, err := schemaform.Build(bt.GetSchemaJson(), cfgPrefix, b.GetSpec().GetConfig().AsMap())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	s.render(w, http.StatusOK, "deploy_form.html", s.editData(w, r, bt, b, fields))
}

func (s *Server) handleEditSubmit(w http.ResponseWriter, r *http.Request) {
	cur, bt, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	before := cur.GetSpec().GetConfig().AsMap()
	cfg, cfgErrs := schemaform.Decode(bt.GetSchemaJson(), cfgPrefix, form)
	keepSecrets(bt.GetSchemaJson(), cfg, before)
	b := proto.Clone(cur).(*pb.Block)
	st, err := structpb.NewStruct(cfg)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	b.Spec.Config = st
	followPorts(b, before)
	sub := formErrors{config: cfgErrs, general: applyCommon(b, form)}
	if sub.admit(r.Context(), s.blocks, b, true) {
		_, err := s.blocks.Update(r.Context(), b)
		if err == nil {
			full := b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()
			s.done(w, r, "success", "Block "+full+" updated", "/blocks/"+full)
			return
		}
		sub.general = append(sub.general, errText(err))
	}
	s.renderRejected(w, r, s.editData(w, r, bt, b, nil), form, sub)
}

// editTarget loads the block named in the path and its catalog type.
func (s *Server) editTarget(w http.ResponseWriter, r *http.Request) (*pb.Block, *pb.BlockType, bool) {
	if s.blocksUnavailable(w, r) || s.catalogUnavailable(w, r) {
		return nil, nil, false
	}
	b, err := s.getBlock(r.Context(), r)
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return nil, nil, false
	}
	bt, err := s.catalog.GetType(r.Context(), &pb.GetTypeRequest{Name: b.GetSpec().GetType()})
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return nil, nil, false
	}
	return b, bt, true
}

func (s *Server) editData(w http.ResponseWriter, r *http.Request, bt *pb.BlockType, b *pb.Block, fields []schemaform.Field) deployFormData {
	full := b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()
	d := s.deployData(w, r, bt, b, fields)
	d.Page = s.newPage(w, r, "Edit "+full, "blocks",
		crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: full, Href: "/blocks/" + full}, crumb{Label: "Edit"})
	d.Edit, d.Action, d.Cancel = true, "/blocks/"+full+"/edit", "/blocks/"+full
	return d
}

// keepSecrets keeps a secret's current value when its input was left empty (it is never shown).
func keepSecrets(schema []byte, cfg, before map[string]any) {
	fields, err := schemaform.Build(schema, cfgPrefix, nil)
	if err != nil {
		return
	}
	for _, f := range fields {
		key := strings.TrimPrefix(f.Path, "/")
		if _, set := cfg[key]; f.Kind == schemaform.Secret && !set && before[key] != nil {
			cfg[key] = before[key]
		}
	}
}

// formErrors sorts a rejected submission's errors by where the form shows them.
type formErrors struct {
	config  []string // "path: message" for a config field
	name    string
	general []string
}

// admit runs the block service's full admission check and reports whether nothing failed.
func (e *formErrors) admit(ctx context.Context, blocks pb.BlockServiceServer, b *pb.Block, update bool) bool {
	if c, ok := blocks.(blockChecker); ok {
		for _, v := range c.Check(ctx, b, update) {
			switch {
			case v.ConfigField != "":
				e.config = append(e.config, v.ConfigField)
			case v.Rule == "V1" || v.Rule == "V2":
				e.name = v.Message
			default:
				e.general = append(e.general, v.Error())
			}
		}
	}
	return len(e.config) == 0 && len(e.general) == 0 && e.name == ""
}

// renderRejected re-renders the form with what was posted and each error on its field.
func (s *Server) renderRejected(w http.ResponseWriter, r *http.Request, data deployFormData, form url.Values, e formErrors) {
	fields, err := schemaform.Build(data.Type.GetSchemaJson(), cfgPrefix, nil)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	schemaform.Refill(fields, form)
	data.Fields = fields
	data.Errors = append(e.general, schemaform.AttachErrors(fields, e.config)...)
	data.NameError = e.name
	if !data.Edit {
		data.Name = form.Get("name")
	}
	data.Replicas, data.CPU, data.Memory = form.Get("replicas"), form.Get("cpu"), form.Get("memory")
	for i := range data.Storage {
		data.Storage[i].Size = form.Get("storage/" + data.Storage[i].Name + "/size")
	}
	s.render(w, http.StatusBadRequest, "deploy_form.html", data)
}

// fitToCluster caps the template's replicas and volume copies (written for 3 nodes) at the node count.
func fitToCluster(b *pb.Block, nodes int) {
	if nodes <= 0 {
		return
	}
	n := int32(nodes)
	if spec := b.GetSpec(); spec.Replicas != nil && spec.GetReplicas() > n {
		spec.Replicas = &n
	}
	for _, st := range b.GetSpec().GetStorage() {
		if st.GetReplication() > n {
			st.Replication = n
		}
	}
}

// followPorts moves port targets and probes from a config port's previous value to its new one.
func followPorts(b *pb.Block, before map[string]any) {
	cfg := b.GetSpec().GetConfig().AsMap()
	moved := map[int32]int32{}
	for k, d := range before {
		from, ok1 := portNumber(d)
		to, ok2 := portNumber(cfg[k])
		if ok1 && ok2 && to != from {
			moved[from] = to
		}
	}
	if len(moved) == 0 {
		return
	}
	net := b.GetSpec().GetNetwork()
	for _, p := range net.GetPorts() {
		if to, ok := moved[p.GetTargetPort()]; ok {
			p.TargetPort = to
		}
	}
	for _, probe := range []*pb.HealthProbe{net.GetHealthCheck().GetReadiness(), net.GetHealthCheck().GetLiveness()} {
		if to, ok := moved[probe.GetPort()]; ok && probe != nil {
			probe.Port = to
		}
	}
}

func portNumber(v any) (int32, bool) {
	switch n := v.(type) {
	case int:
		return int32(n), true
	case int64:
		return int32(n), true
	case float64:
		return int32(n), n == float64(int32(n))
	}
	return 0, false
}

// applyCommon writes the form's replicas, resources and storage sizes into b.
func applyCommon(b *pb.Block, form url.Values) []string {
	var errs []string
	spec := b.GetSpec()
	if v := strings.TrimSpace(form.Get("replicas")); v != "" && showReplicas(spec) {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			errs = append(errs, "Replicas must be a whole number")
		} else {
			r := int32(n)
			spec.Replicas = &r
		}
	}
	cpu, mem := strings.TrimSpace(form.Get("cpu")), strings.TrimSpace(form.Get("memory"))
	if cpu != "" || mem != "" {
		if spec.Resources == nil {
			spec.Resources = &pb.Resources{}
		}
		if spec.Resources.Requests == nil {
			spec.Resources.Requests = &pb.ResourcePair{}
		}
		if cpu != "" {
			spec.Resources.Requests.Cpu = cpu
		}
		if mem != "" {
			spec.Resources.Requests.Memory = mem
		}
	}
	for _, st := range spec.GetStorage() {
		if v := strings.TrimSpace(form.Get("storage/" + st.GetName() + "/size")); v != "" {
			st.Size = v
		}
	}
	return errs
}

func showReplicas(spec *pb.BlockSpec) bool {
	k := spec.GetStrategy().GetKind()
	return k != pb.StrategyKind_SINGLETON && k != pb.StrategyKind_DAEMONSET
}

func (s *Server) deployData(w http.ResponseWriter, r *http.Request, bt *pb.BlockType, b *pb.Block, fields []schemaform.Field) deployFormData {
	spec := b.GetSpec()
	d := deployFormData{
		Page: s.newPage(w, r, "Deploy "+bt.GetName(), "blocks",
			crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: "Deploy", Href: "/deploy"}, crumb{Label: bt.GetName()}),
		Type: bt, Action: "/deploy/" + bt.GetName(), Cancel: "/deploy", Name: b.GetMetadata().GetName(),
		ShowReplicas: showReplicas(spec), CPU: spec.GetResources().GetRequests().GetCpu(),
		Memory: spec.GetResources().GetRequests().GetMemory(), Ports: spec.GetNetwork().GetPorts(), Fields: fields,
	}
	if spec.Replicas != nil {
		d.Replicas = strconv.Itoa(int(spec.GetReplicas()))
	}
	for _, st := range spec.GetStorage() {
		d.Storage = append(d.Storage, storageInput{Name: st.GetName(), Size: st.GetSize(), Replication: st.GetReplication()})
	}
	d.Manifest = manifestYAML(b, fields)
	return d
}

// manifestYAML renders b as the YAML `expanse ctl block apply -f` takes, with secrets hidden.
func manifestYAML(b *pb.Block, fields []schemaform.Field) string {
	raw, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(&pb.Block{Metadata: b.GetMetadata(), Spec: b.GetSpec()})
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if cfg, ok := doc["spec"].(map[string]any)["config"].(map[string]any); ok {
		for _, f := range fields {
			if _, set := cfg[strings.TrimPrefix(f.Path, "/")]; set && f.Kind == schemaform.Secret {
				cfg[strings.TrimPrefix(f.Path, "/")] = "<hidden>"
			}
		}
	}
	out, err := yaml.Marshal(map[string]any{"apiVersion": "expanse.io/v1", "kind": "Block", "metadata": doc["metadata"], "spec": doc["spec"]})
	if err != nil {
		return ""
	}
	return string(out)
}
