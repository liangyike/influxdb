package pkger

import (
	"context"
	"net/http"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/pkg/httpc"
)

// HTTPRemoteService provides an http client that is fluent in all things pkger.
type HTTPRemoteService struct {
	Client *httpc.Client
}

var _ SVC = (*HTTPRemoteService)(nil)

func (s *HTTPRemoteService) InitStack(ctx context.Context, userID influxdb.ID, stack Stack) (Stack, error) {
	reqBody := ReqCreateStack{
		OrgID:       stack.OrgID.String(),
		Name:        stack.Name,
		Description: stack.Description,
		URLs:        stack.URLs,
	}

	var respBody RespCreateStack
	err := s.Client.
		PostJSON(reqBody, RoutePrefix, "/stacks").
		DecodeJSON(&respBody).
		Do(ctx)
	if err != nil {
		return Stack{}, err
	}

	newStack := Stack{
		Name:        respBody.Name,
		Description: respBody.Description,
		URLs:        respBody.URLs,
		Resources:   make([]StackResource, 0),
		CRUDLog:     respBody.CRUDLog,
	}

	id, err := influxdb.IDFromString(respBody.ID)
	if err != nil {
		return Stack{}, err
	}
	newStack.ID = *id

	orgID, err := influxdb.IDFromString(respBody.OrgID)
	if err != nil {
		return Stack{}, err
	}
	newStack.OrgID = *orgID

	return newStack, nil
}

func (s *HTTPRemoteService) DeleteStack(ctx context.Context, identifiers struct{ OrgID, UserID, StackID influxdb.ID }) error {
	return s.Client.
		Delete(RoutePrefix, "stacks", identifiers.StackID.String()).
		QueryParams([2]string{"orgID", identifiers.OrgID.String()}).
		Do(ctx)
}

func (s *HTTPRemoteService) ExportStack(ctx context.Context, orgID, stackID influxdb.ID) (*Pkg, error) {
	pkg := new(Pkg)
	err := s.Client.
		Get(RoutePrefix, "stacks", stackID.String(), "export").
		QueryParams([2]string{"orgID", orgID.String()}).
		Decode(func(resp *http.Response) error {
			decodedPkg, err := Parse(EncodingJSON, FromReader(resp.Body))
			if err != nil {
				return err
			}
			pkg = decodedPkg
			return nil
		}).
		Do(ctx)
	if err != nil {
		return nil, err
	}

	if err := pkg.Validate(ValidWithoutResources()); err != nil {
		return nil, err
	}
	return pkg, nil
}

func (s *HTTPRemoteService) ListStacks(ctx context.Context, orgID influxdb.ID, f ListFilter) ([]Stack, error) {
	queryParams := [][2]string{{"orgID", orgID.String()}}
	for _, name := range f.Names {
		queryParams = append(queryParams, [2]string{"name", name})
	}
	for _, stackID := range f.StackIDs {
		queryParams = append(queryParams, [2]string{"stackID", stackID.String()})
	}

	var resp RespListStacks
	err := s.Client.
		Get(RoutePrefix, "/stacks").
		QueryParams(queryParams...).
		DecodeJSON(&resp).
		Do(ctx)
	if err != nil {
		return nil, err
	}
	return resp.Stacks, nil
}

// CreatePkg will produce a pkg from the parameters provided.
func (s *HTTPRemoteService) CreatePkg(ctx context.Context, setters ...CreatePkgSetFn) (*Pkg, error) {
	var opt CreateOpt
	for _, setter := range setters {
		if err := setter(&opt); err != nil {
			return nil, err
		}
	}

	var orgIDs []ReqCreateOrgIDOpt
	for _, org := range opt.OrgIDs {
		orgIDs = append(orgIDs, ReqCreateOrgIDOpt{
			OrgID: org.OrgID.String(),
			Filters: struct {
				ByLabel        []string `json:"byLabel"`
				ByResourceKind []Kind   `json:"byResourceKind"`
			}{
				ByLabel:        org.LabelNames,
				ByResourceKind: org.ResourceKinds,
			},
		})
	}

	reqBody := ReqCreatePkg{
		OrgIDs:    orgIDs,
		Resources: opt.Resources,
	}

	var newPkg *Pkg
	err := s.Client.
		PostJSON(reqBody, RoutePrefix).
		Decode(func(resp *http.Response) error {
			pkg, err := Parse(EncodingJSON, FromReader(resp.Body))
			newPkg = pkg
			return err
		}).
		Do(ctx)
	if err != nil {
		return nil, err
	}

	if err := newPkg.Validate(ValidWithoutResources()); err != nil {
		return nil, err
	}
	return newPkg, nil
}

// DryRun provides a dry run of the pkg application. The pkg will be marked verified
// for later calls to Apply. This func will be run on an Apply if it has not been run
// already.
func (s *HTTPRemoteService) DryRun(ctx context.Context, orgID, userID influxdb.ID, pkg *Pkg, opts ...ApplyOptFn) (PkgImpactSummary, error) {
	return s.apply(ctx, orgID, pkg, true, opts...)
}

// Apply will apply all the resources identified in the provided pkg. The entire pkg will be applied
// in its entirety. If a failure happens midway then the entire pkg will be rolled back to the state
// from before the pkg was applied.
func (s *HTTPRemoteService) Apply(ctx context.Context, orgID, userID influxdb.ID, pkg *Pkg, opts ...ApplyOptFn) (PkgImpactSummary, error) {
	return s.apply(ctx, orgID, pkg, false, opts...)
}

func (s *HTTPRemoteService) apply(ctx context.Context, orgID influxdb.ID, pkg *Pkg, dryRun bool, opts ...ApplyOptFn) (PkgImpactSummary, error) {
	opt := applyOptFromOptFns(opts...)

	var rawPkg []byte
	if pkg != nil {
		b, err := pkg.Encode(EncodingJSON)
		if err != nil {
			return PkgImpactSummary{}, err
		}
		rawPkg = b
	}

	reqBody := ReqApplyPkg{
		OrgID:   orgID.String(),
		DryRun:  dryRun,
		EnvRefs: opt.EnvRefs,
		Secrets: opt.MissingSecrets,
		RawPkg:  rawPkg,
	}
	if opt.StackID != 0 {
		stackID := opt.StackID.String()
		reqBody.StackID = &stackID
	}

	var resp RespApplyPkg
	err := s.Client.
		PostJSON(reqBody, RoutePrefix, "/apply").
		DecodeJSON(&resp).
		Do(ctx)
	if err != nil {
		return PkgImpactSummary{}, err
	}

	impact := PkgImpactSummary{
		Diff:    resp.Diff,
		Summary: resp.Summary,
	}

	if stackID, err := influxdb.IDFromString(resp.StackID); err == nil {
		impact.StackID = *stackID
	}

	return impact, NewParseError(resp.Errors...)
}
