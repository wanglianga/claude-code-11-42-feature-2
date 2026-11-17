package domain

import (
	"fmt"
	"sort"
	"time"
)

// Solver resource ceilings. The platform evaluates on demand and accepts that
// a deep chain may not answer immediately; hitting a ceiling returns
// "unverified" rather than presenting one feasible allocation as if it were
// the exact boundary.
const (
	DefaultAugmentLimit = 200000
	DefaultVertexLimit  = 20000
)

// LineageNode is one vertex in an expanded pedigree graph.
type LineageNode struct {
	NodeID       string    `json:"node_id"`
	Kind         string    `json:"kind"` // source|package|operation|container
	Label        string    `json:"label,omitempty"`
	InstanceID   string    `json:"instance_id,omitempty"`
	OperationKey string    `json:"operation_key,omitempty"`
	Station      string    `json:"station,omitempty"`
	ActiveFrom   time.Time `json:"active_from,omitempty"`
	Role         string    `json:"role,omitempty"`
}

// LineageEdge is one explained relationship. Quantity flow only exists on
// split/mixed_repack edges; stow/relabel edges carry their own semantics.
type LineageEdge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Kind       string `json:"kind"` // flow|relabel|stow|produce|consume
	Operation  string `json:"operation,omitempty"`
	LowerQty   int64  `json:"lower_qty,omitempty"`
	UpperQty   int64  `json:"upper_qty,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Pinned     bool   `json:"pinned,omitempty"`
	HasBasis   bool   `json:"has_basis"`
	BasisEvent string `json:"basis_event_id,omitempty"`
}

// Expansion is the lineage sub-graph reaching one current package.
type Expansion struct {
	TargetInstanceID string         `json:"target_instance_id"`
	Nodes            []LineageNode  `json:"nodes"`
	Edges            []LineageEdge  `json:"edges"`
	OriginalSources  []string       `json:"original_source_instance_ids"`
	Depth            int            `json:"max_depth"`
	Containment      []StowInterval `json:"containment_intervals,omitempty"`
}

// participation is a flow-network vertex key: one side of one operation.
type participation struct {
	op   string
	inst string
	side string // in|out
}

// SourceRangeQuery pins one original source and one surviving target.
type SourceRangeQuery struct {
	SourceInstanceID string
	TargetInstanceID string
	Unit             string // requested answer unit; defaults to the chain unit
	AugmentLimit     int
	VertexLimit      int
}

// SourceRange is the exact feasible range answer.
type SourceRange struct {
	SourceInstanceID string        `json:"source_instance_id"`
	TargetInstanceID string        `json:"target_instance_id"`
	Unit             string        `json:"unit"`
	Family           string        `json:"family"`
	Lower            int64         `json:"lower"`
	Upper            int64         `json:"upper"`
	Status           string        `json:"status"` // exact|partial|uncertain|absent|infeasible|unverified
	MissingBasis     []string      `json:"missing_basis,omitempty"`
	ConflictingOps   []string      `json:"conflicting_operations,omitempty"`
	Witness          []WitnessFlow `json:"witness_allocation,omitempty"`
	ResourceLimit    bool          `json:"resource_limit_reached,omitempty"`
	Notes            []string      `json:"notes,omitempty"`
	Computed         bool          `json:"computed"`
}

// canonicalGroup unifies instances joined by documented relabel aliases. A
// relabel adds no goods, so the from/to instances share one flow identity.
type canonicalGroup struct{ parent map[string]string }

func newCanonicalGroup(aliases []AliasRecord) *canonicalGroup {
	g := &canonicalGroup{parent: map[string]string{}}
	find := func(x string) string {
		g.parent[x] = x
		for g.parent[x] != x {
			g.parent[x] = g.parent[g.parent[x]]
			x = g.parent[x]
		}
		return x
	}
	for _, a := range aliases {
		if a.BasisEventID == "" {
			continue // an alias without a basis is not accepted as identity
		}
		ra, rb := find(a.FromInstanceID), find(a.ToInstanceID)
		if ra != rb {
			g.parent[rb] = ra
		}
	}
	return g
}

func (g *canonicalGroup) find(x string) string {
	if _, ok := g.parent[x]; !ok {
		g.parent[x] = x
	}
	for g.parent[x] != x {
		g.parent[x] = g.parent[g.parent[x]]
		x = g.parent[x]
	}
	return x
}

// flowOps returns the operations that move quantities, in chain order.
func (m *PedigreeModel) flowOps() []OperationRecord {
	out := make([]OperationRecord, 0, len(m.OpOrder))
	for _, k := range m.OpOrder {
		op := m.Operations[k]
		if op.Kind == OpSplit || op.Kind == OpMixedRepack {
			out = append(out, op)
		}
	}
	return out
}

// blockerOps returns operations participating in an unsatisfiable constraint
// set: conservation failures, over-capacity allocations and cycles.
func (m *PedigreeModel) blockerOps() []string {
	set := map[string]bool{}
	for _, f := range m.findings {
		switch f.Code {
		case FindConservationMismatch, FindAllocExceeds, FindOpCycle, FindSelfReflow:
			if f.OperationKey != "" {
				set[f.OperationKey] = true
			}
			for _, k := range f.RelatedOps {
				set[k] = true
			}
		}
	}
	out := mapKeys(set)
	sort.Strings(out)
	return out
}

// networkBuild keeps the constructed graph plus vertex maps for queries.
type networkBuild struct {
	net         *FlowNetwork
	vertex      map[participation]int
	originOf    map[string]participation
	terminalOf  map[string]participation
	supplies    map[int]int64
	demands     map[int]int64
	totalSupply int64
	totalDemand int64
	unit        string
	family      string
	baseUnit    string
	totalQty    int64
}

func (m *PedigreeModel) buildNetwork() (*networkBuild, error) {
	group := newCanonicalGroup(m.Aliases)
	ops := m.flowOps()
	if len(ops) == 0 {
		return &networkBuild{}, nil
	}
	// All flow ops reconcile in one declared unit family; mixed families stay
	// pending and are reported by the model findings.
	unit := ops[0].Unit
	family, known := m.Units.FamilyOf(unit)
	if !known {
		return nil, fmt.Errorf("reconciliation unit %q is not registered", unit)
	}
	vertex := map[participation]int{}
	nextID := 0
	ensure := func(p participation) int {
		if id, ok := vertex[p]; ok {
			return id
		}
		id := nextID
		nextID++
		vertex[p] = id
		return id
	}
	// Pre-register every participation so ids are stable across edge inserts.
	var totalQty int64
	for _, op := range ops {
		for _, q := range op.Inputs {
			ensure(participation{op.OperationKey, group.find(q.InstanceID), "in"})
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				totalQty += v
			}
		}
		for _, q := range op.Outputs {
			ensure(participation{op.OperationKey, group.find(q.InstanceID), "out"})
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				totalQty += v
			}
		}
	}
	if nextID > DefaultVertexLimit {
		return nil, fmt.Errorf("vertex limit %d exceeded", DefaultVertexLimit)
	}
	net := NewFlowNetwork(nextID, totalQty+1)

	// Which canonical instances are produced / consumed by flow operations.
	produced := map[string]bool{}
	consumed := map[string]bool{}
	for _, op := range ops {
		for _, q := range op.Outputs {
			produced[group.find(q.InstanceID)] = true
		}
		for _, q := range op.Inputs {
			consumed[group.find(q.InstanceID)] = true
		}
	}

	// in-op allocation edges, including zero-lower edges for every feasible
	// (input, output) pair. A reachable-but-unallocated pair is exactly what
	// produces a 0..N uncertain range rather than a guessed share.
	for _, op := range ops {
		inCap := map[string]int64{}
		outCap := map[string]int64{}
		for _, q := range op.Inputs {
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				inCap[group.find(q.InstanceID)] += v
			}
		}
		for _, q := range op.Outputs {
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				outCap[group.find(q.InstanceID)] += v
			}
		}
		lowers := map[string]int64{}
		pinned := map[string]bool{}
		basis := map[string]string{}
		for _, a := range op.Allocations {
			fam, ok := m.Units.FamilyOf(a.Unit)
			if !ok || fam != family {
				continue
			}
			v, _, _ := m.Units.ToBase(a.Unit, a.Quantity)
			k := group.find(a.FromInstanceID) + "->" + group.find(a.ToInstanceID)
			lowers[k] += v
			pinned[k] = true
			basis[k] = a.EvidenceEventID
		}
		ins := make([]string, 0, len(inCap))
		for id := range inCap {
			ins = append(ins, id)
		}
		outs := make([]string, 0, len(outCap))
		for id := range outCap {
			outs = append(outs, id)
		}
		sort.Strings(ins)
		sort.Strings(outs)
		for _, ii := range ins {
			for _, oo := range outs {
				k := ii + "->" + oo
				upper := inCap[ii]
				if outCap[oo] < upper {
					upper = outCap[oo]
				}
				lower := lowers[k]
				if lower > upper {
					// Keep the edge but carry the infeasibility; the global
					// feasibility step will reject it and blockerOps names the
					// operation instead of silently clipping the bound.
					upper = lower
				}
				// An evidenced explicit allocation pins the edge: its quantity
				// is both the required lower bound and the only admissible
				// capacity, so a later voucher cannot be silently exceeded and
				// a feasible allocation never adds unevidenced flow to it.
				edgeLower, edgeUpper := lower, upper
				if pinned[k] {
					edgeLower, edgeUpper = lower, lower
				}
				if err := net.AddEdge(
					ensure(participation{op.OperationKey, ii, "in"}),
					ensure(participation{op.OperationKey, oo, "out"}),
					edgeLower, edgeUpper, EdgeMeta{
						OpKey: op.OperationKey, From: ii, To: oo,
						Kind: "allocation", Unit: unit, Pinned: pinned[k],
					}); err != nil {
					return nil, err
				}
			}
		}
	}

	// Carry edges: an output of one op feeding an input of a later op.
	firstConsumer := map[string]participation{}
	lastProducer := map[string]participation{}
	for _, op := range ops {
		for _, q := range op.Inputs {
			c := group.find(q.InstanceID)
			if _, ok := firstConsumer[c]; !ok {
				firstConsumer[c] = participation{op.OperationKey, c, "in"}
			}
		}
		for _, q := range op.Outputs {
			c := group.find(q.InstanceID)
			lastProducer[c] = participation{op.OperationKey, c, "out"}
		}
	}
	for c, out := range lastProducer {
		in, ok := firstConsumer[c]
		if !ok || in.op == out.op {
			continue
		}
		if v, fam, ok := m.instanceCarryCap(c, family); ok && fam == family {
			if err := net.AddEdge(vertex[out], vertex[in], 0, v, EdgeMeta{
				From: c, To: c, Kind: "carry", Unit: unit,
			}); err != nil {
				return nil, err
			}
		}
	}

	// Supplies at original-source participations and demands at surviving
	// participations, encoded as node b-values in the flow engine. An instance
	// produced by an earlier op is supplied through a carry edge, never
	// re-injected, so no quantity is invented inside the chain.
	supplies := map[int]int64{}
	demands := map[int]int64{}
	totalSupply, totalDemand := int64(0), int64(0)
	supplyCap := map[string]int64{}
	for _, op := range ops {
		for _, q := range op.Inputs {
			c := group.find(q.InstanceID)
			if produced[c] {
				continue
			}
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				supplyCap[c] += v
			}
		}
	}
	originOf := map[string]participation{}
	for c, capV := range supplyCap {
		var origin participation
		for _, op := range ops {
			for _, q := range op.Inputs {
				if group.find(q.InstanceID) == c {
					origin = participation{op.OperationKey, c, "in"}
					break
				}
			}
			if origin.op != "" {
				break
			}
		}
		originOf[c] = origin
		supplies[vertex[origin]] = capV
		totalSupply += capV
	}
	sinkCap := map[string]int64{}
	for _, op := range ops {
		for _, q := range op.Outputs {
			c := group.find(q.InstanceID)
			if consumed[c] {
				continue // carried onward, not terminal
			}
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				sinkCap[c] += v
			}
		}
	}
	terminalOf := map[string]participation{}
	for c, capV := range sinkCap {
		var terminal participation
		for _, op := range ops {
			for _, q := range op.Outputs {
				if group.find(q.InstanceID) == c {
					terminal = participation{op.OperationKey, c, "out"}
				}
			}
		}
		terminalOf[c] = terminal
		demands[vertex[terminal]] = capV
		totalDemand += capV
	}

	build := &networkBuild{
		net: net, vertex: vertex,
		originOf:    originOf,
		terminalOf:  terminalOf,
		supplies:    supplies,
		demands:     demands,
		totalSupply: totalSupply,
		totalDemand: totalDemand,
		unit:        unit, family: family, totalQty: totalQty,
	}
	// base unit label for reporting
	if _, base, ok := m.Units.ToBase(unit, 1); ok {
		build.baseUnit = base
	}
	return build, nil
}

// instanceCarryCap is the item quantity a package holds when it leaves one
// operation, i.e. the tightest declared output quantity in the family.
func (m *PedigreeModel) instanceCarryCap(canonical string, family string) (int64, string, bool) {
	var best int64
	found := false
	for _, op := range m.flowOps() {
		for _, q := range op.Outputs {
			if q.InstanceID != canonical {
				continue
			}
			if v, fam, ok := m.Units.ToBase(q.Unit, q.Quantity); ok && fam == family {
				if !found || v < best {
					best, found = v, true
				}
			}
		}
	}
	return best, family, found
}

// SourceRange solves the feasible integral range for one pair.
func (m *PedigreeModel) SourceRange(q SourceRangeQuery) SourceRange {
	limit := q.AugmentLimit
	if limit <= 0 {
		limit = DefaultAugmentLimit
	}
	res := SourceRange{
		SourceInstanceID: q.SourceInstanceID, TargetInstanceID: q.TargetInstanceID,
		MissingBasis: []string{}, ConflictingOps: []string{}, Notes: []string{},
	}
	group := newCanonicalGroup(m.Aliases)
	src := group.find(q.SourceInstanceID)
	tgt := group.find(q.TargetInstanceID)
	res.SourceInstanceID, res.TargetInstanceID = src, tgt
	if _, ok := m.Instances[q.SourceInstanceID]; !ok {
		res.Status = "infeasible"
		res.Notes = append(res.Notes, "source instance is not registered")
		return res
	}
	if _, ok := m.Instances[q.TargetInstanceID]; !ok {
		res.Status = "infeasible"
		res.Notes = append(res.Notes, "target instance is not registered")
		return res
	}
	build, err := m.buildNetwork()
	if err != nil || build.net == nil {
		res.Status = "unverified"
		if err != nil {
			res.Notes = append(res.Notes, err.Error())
		}
		return res
	}
	res.Unit, res.Family = build.unit, build.family
	if q.Unit != "" {
		if fam, ok := m.Units.FamilyOf(q.Unit); !ok || fam != build.family {
			res.Status = "unverified"
			res.Notes = append(res.Notes, "requested unit "+q.Unit+" is not convertible to the chain family")
			return res
		}
		res.Unit = q.Unit
	}
	// Hard constraint failures make the whole chain infeasible; name the set.
	if blockers := m.blockerOps(); len(blockers) > 0 {
		res.Status = "infeasible"
		res.ConflictingOps = blockers
		res.Notes = append(res.Notes, "constraint contradictions make the full-chain allocation unsatisfiable")
		return res
	}

	// The origin/terminal participations were fixed while building the network
	// (S0 supplies origins, T0 drains terminals). A source that was itself
	// produced by an earlier op is not an origin; a target consumed again is
	// not a terminal.
	sPart, sFound := build.originOf[src]
	tPart, tFound := build.terminalOf[tgt]
	if !sFound || !tFound {
		res.Status = "absent"
		if !sFound {
			res.Notes = append(res.Notes, "source never enters a quantity-flow operation as an original input")
		}
		if !tFound {
			res.Notes = append(res.Notes, "target is not a surviving output of the flow chain")
		}
		return res
	}
	sID, ok1 := build.vertex[sPart]
	tID, ok2 := build.vertex[tPart]
	if !ok1 || !ok2 {
		res.Status = "unverified"
		res.Notes = append(res.Notes, "internal vertex mapping failed")
		return res
	}
	net := build.net.Clone()
	bounds := net.MinMaxPairFlow(sID, tID, build.supplies, build.demands, limit)
	res.Lower, res.Upper, res.Witness = bounds.Lower, bounds.Upper, bounds.Witness
	res.ResourceLimit = bounds.ResourceLimit
	res.Status = bounds.Status
	if bounds.Status == "infeasible" {
		res.ConflictingOps = m.blockerOps()
		res.Notes = append(res.Notes, bounds.InfeasibleNotes...)
		return res
	}
	if bounds.Status == "unverified" {
		res.Notes = append(res.Notes, bounds.InfeasibleNotes...)
		// An unfinished solve must never present a found allocation as a bound.
		res.Lower, res.Upper, res.Witness = 0, 0, nil
		return res
	}
	res.Computed = true
	// Convert base-unit bounds into the requested unit when a factor exists.
	if q.Unit != "" && q.Unit != build.unit {
		if d, ok := m.Units.defUnit(q.Unit); ok && d.Factor > 0 {
			res.Lower /= d.Factor
			res.Upper /= d.Factor
		}
	}
	// Whenever the range is not pinned to an exact value, name the evidence
	// missing along the source->target path instead of implying certainty. A
	// lower>0/upper>lower partial range is still an open range.
	if res.Lower < res.Upper {
		res.MissingBasis = m.missingBasisOnPath(src, tgt)
	}
	return res
}

// defUnit exposes a unit definition for reporting conversions.
func (r *UnitRegistry) defUnit(unit string) (UnitDef, bool) {
	d, ok := r.defs[unit]
	return d, ok
}

// missingBasisOnPath lists flow operations on any source->target route that
// declare no explicit allocation for the involved pair. These are precisely
// the documents an auditor would need to close the open range.
func (m *PedigreeModel) missingBasisOnPath(src, tgt string) []string {
	group := newCanonicalGroup(m.Aliases)
	// Adjacency over canonical instances through flow operations.
	adj := map[string]map[string]string{} // from -> to -> op (first)
	for _, op := range m.flowOps() {
		hasAlloc := map[string]bool{}
		for _, a := range op.Allocations {
			hasAlloc[group.find(a.FromInstanceID)+"->"+group.find(a.ToInstanceID)] = true
		}
		for _, in := range op.Inputs {
			for _, out := range op.Outputs {
				i, o := group.find(in.InstanceID), group.find(out.InstanceID)
				k := i + "->" + o
				if hasAlloc[k] {
					continue
				}
				if adj[i] == nil {
					adj[i] = map[string]string{}
				}
				if _, ok := adj[i][o]; !ok {
					adj[i][o] = op.OperationKey
				}
			}
		}
	}
	// BFS all routes; every unallocated op on any route is a missing basis.
	seen := map[string]bool{}
	queue := []string{src}
	ops := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for nx, op := range adj[cur] {
			ops[op] = true
			queue = append(queue, nx)
		}
	}
	out := mapKeys(ops)
	sort.Strings(out)
	return out
}

// Expand builds the lineage sub-graph that reaches targetInstance. Intact
// stowage is included as containment (no quantity flow), relabels as alias
// edges, and split/mixed_repack as quantity-flow edges with their current
// feasible bounds filled lazily by the caller via SourceRange.
func (m *PedigreeModel) Expand(targetInstance string) Expansion {
	group := newCanonicalGroup(m.Aliases)
	tgtCanon := group.find(targetInstance)
	exp := Expansion{
		TargetInstanceID: tgtCanon,
		Nodes:            []LineageNode{}, Edges: []LineageEdge{},
		OriginalSources: []string{},
	}
	nodeSeen := map[string]bool{}
	addNode := func(n LineageNode) {
		if nodeSeen[n.NodeID] {
			return
		}
		nodeSeen[n.NodeID] = true
		exp.Nodes = append(exp.Nodes, n)
	}
	edgeSeen := map[string]bool{}
	addEdge := func(e LineageEdge) {
		k := e.Kind + "|" + e.From + "|" + e.To + "|" + e.Operation
		if edgeSeen[k] {
			return
		}
		edgeSeen[k] = true
		exp.Edges = append(exp.Edges, e)
	}
	// Reverse walk over flow operations from the target, bounded by visited ops.
	visitedOps := map[string]bool{}
	frontier := []string{tgtCanon}
	depth := 0
	for len(frontier) > 0 {
		next := []string{}
		depth++
		for _, canon := range frontier {
			producers := m.producerOps(canon)
			if len(producers) == 0 {
				if ins, ok := m.Instances[canon]; ok {
					addNode(LineageNode{
						NodeID: "inst:" + canon, Kind: "source", InstanceID: canon,
						Label: ins.Label, Role: ins.Role, ActiveFrom: ins.ActiveFrom,
					})
					exp.OriginalSources = append(exp.OriginalSources, canon)
				}
				continue
			}
			for _, opKey := range producers {
				if visitedOps[opKey] {
					continue
				}
				visitedOps[opKey] = true
				op := m.Operations[opKey]
				addNode(LineageNode{
					NodeID: "op:" + opKey, Kind: "operation", OperationKey: opKey,
					Station: op.Station, Role: op.Kind,
				})
				for _, q := range op.Outputs {
					o := group.find(q.InstanceID)
					addNode(LineageNode{NodeID: "inst:" + o, Kind: "package", InstanceID: o,
						Label: m.labelOf(o), ActiveFrom: op.OccurredAt})
					addEdge(LineageEdge{From: "op:" + opKey, To: "inst:" + o, Kind: "flow",
						Operation: opKey, Unit: op.Unit, HasBasis: true})
					if o == canon {
						next = append(next, o)
					}
				}
				allocated := map[string]bool{}
				for _, a := range op.Allocations {
					allocated[group.find(a.FromInstanceID)+"->"+group.find(a.ToInstanceID)] = true
				}
				for _, q := range op.Inputs {
					i := group.find(q.InstanceID)
					addNode(LineageNode{NodeID: "inst:" + i, Kind: "package", InstanceID: i,
						Label: m.labelOf(i)})
					addEdge(LineageEdge{
						From: "inst:" + i, To: "op:" + opKey, Kind: "flow", Operation: opKey,
						Unit: op.Unit, HasBasis: true,
						Pinned: allocated[i+"->"+canon],
					})
					next = append(next, i)
				}
			}
		}
		frontier = uniqueSorted(next)
	}
	exp.Depth = depth
	// Alias edges.
	for _, a := range m.Aliases {
		if a.BasisEventID == "" {
			continue
		}
		from, to := group.find(a.FromInstanceID), group.find(a.ToInstanceID)
		if nodeSeen["inst:"+from] || nodeSeen["inst:"+to] {
			addNode(LineageNode{NodeID: "inst:" + from, Kind: "package", InstanceID: from, Label: m.labelOf(from)})
			addNode(LineageNode{NodeID: "inst:" + to, Kind: "package", InstanceID: to, Label: m.labelOf(to)})
			addEdge(LineageEdge{From: "inst:" + from, To: "inst:" + to, Kind: "relabel",
				HasBasis: true, BasisEvent: a.BasisEventID})
		}
	}
	// Containment edges (intact stowage) touching any expanded instance.
	inSet := map[string]bool{}
	for _, n := range exp.Nodes {
		if n.InstanceID != "" {
			inSet[n.InstanceID] = true
		}
	}
	for _, s := range m.Stows {
		if inSet[s.ChildInstanceID] {
			addNode(LineageNode{NodeID: "loc:" + s.ParentRef, Kind: "container", Label: s.ParentRef})
			addEdge(LineageEdge{From: "inst:" + s.ChildInstanceID, To: "loc:" + s.ParentRef,
				Kind: "stow", Operation: s.SourceEventID, HasBasis: s.HandoverEventID != "",
				BasisEvent: s.HandoverEventID})
			exp.Containment = append(exp.Containment, s)
		}
	}
	exp.OriginalSources = uniqueSorted(exp.OriginalSources)
	sort.Slice(exp.Nodes, func(i, j int) bool { return exp.Nodes[i].NodeID < exp.Nodes[j].NodeID })
	sort.Slice(exp.Edges, func(i, j int) bool {
		if exp.Edges[i].From != exp.Edges[j].From {
			return exp.Edges[i].From < exp.Edges[j].From
		}
		return exp.Edges[i].To < exp.Edges[j].To
	})
	return exp
}

func (m *PedigreeModel) producerOps(canon string) []string {
	set := map[string]bool{}
	for id, opKey := range m.producers {
		g := newCanonicalGroup(m.Aliases)
		if g.find(id) == canon {
			set[opKey] = true
		}
	}
	out := mapKeys(set)
	sort.Strings(out)
	return out
}

func (m *PedigreeModel) labelOf(canon string) string {
	if ins, ok := m.Instances[canon]; ok {
		return ins.Label
	}
	return canon
}

func uniqueSorted(in []string) []string {
	out := SortedUnique(in)
	if out == nil {
		return []string{}
	}
	return out
}

// ---- Nested containment and non-double-counting summaries ------------------

// ContentsAt resolves everything inside a container/outer package at time t,
// traversing only stow intervals effective then. Item totals count LEAF
// packages only, so an outer box and its inner pieces are never added twice;
// package counts are reported as a separate family.
type ContentsSummary struct {
	ParentRef       string     `json:"parent_ref"`
	At              time.Time  `json:"at"`
	LeafPackages    []string   `json:"leaf_packages"`
	AllDescendants  []string   `json:"all_descendants"`
	ItemTotal       int64      `json:"item_total"`
	ItemUnit        string     `json:"item_unit,omitempty"`
	PackageCount    int        `json:"package_count"`
	PendingUnits    []string   `json:"pending_units,omitempty"`
	ContainmentPath [][]string `json:"containment_paths"`
}

// ContentsAt walks active stow intervals transitively.
func (m *PedigreeModel) ContentsAt(parentRef string, t time.Time) ContentsSummary {
	res := ContentsSummary{ParentRef: parentRef, At: t, LeafPackages: []string{}, AllDescendants: []string{}}
	childrenOf := map[string][]string{}
	active := map[string]StowInterval{}
	for _, s := range m.Stows {
		if s.ActiveAt(t) {
			childrenOf[s.ParentRef] = append(childrenOf[s.ParentRef], s.ChildInstanceID)
			active[s.ChildInstanceID] = s
		}
	}
	visited := map[string]bool{}
	var walk func(parent string, path []string)
	walk = func(parent string, path []string) {
		for _, child := range childrenOf[parent] {
			if visited[child] {
				continue
			}
			visited[child] = true
			p := append(append([]string{}, path...), child)
			res.AllDescendants = append(res.AllDescendants, child)
			if len(childrenOf[child]) == 0 {
				res.LeafPackages = append(res.LeafPackages, child)
				res.ContainmentPath = append(res.ContainmentPath, p)
			} else {
				walk(child, p)
			}
		}
	}
	walk(parentRef, []string{parentRef})
	res.LeafPackages = SortedUnique(res.LeafPackages)
	res.AllDescendants = SortedUnique(res.AllDescendants)
	res.PackageCount = len(res.AllDescendants)
	// Item totals come from the package's last producing output quantity (the
	// amount it physically holds), never from outer packages.
	unit := ""
	pending := map[string]bool{}
	for _, leaf := range res.LeafPackages {
		qty, u, ok := m.holdingQuantity(leaf)
		if !ok {
			pending[leaf] = true
			continue
		}
		if unit == "" {
			unit = u
		}
		if u != unit {
			pending[leaf] = true
			continue
		}
		res.ItemTotal += qty
	}
	res.ItemUnit = unit
	for id := range pending {
		res.PendingUnits = append(res.PendingUnits, id)
	}
	sort.Strings(res.PendingUnits)
	return res
}

// holdingQuantity returns the inner-item quantity a package currently holds,
// preferring its most recent flow-op output declaration.
func (m *PedigreeModel) holdingQuantity(canon string) (int64, string, bool) {
	var best int64
	var unit string
	found := false
	var bestAt time.Time
	for _, op := range m.flowOps() {
		for _, q := range op.Outputs {
			g := newCanonicalGroup(m.Aliases)
			if g.find(q.InstanceID) != canon {
				continue
			}
			if fam, ok := m.Units.FamilyOf(q.Unit); ok && fam == FamilyItem {
				if !found || op.OccurredAt.After(bestAt) {
					v, _, _ := m.Units.ToBase(q.Unit, q.Quantity)
					best, unit, found, bestAt = v, q.Unit, true, op.OccurredAt
				}
			}
		}
	}
	return best, unit, found
}

// ---- Scan attribution -------------------------------------------------------

const (
	ScanAttributed      = "attributed"     // 绑定到扫描时有效的实例
	ScanUnattributable  = "unattributable" // 不能归到具体子件
	ScanUnknownInstance = "unknown_instance"
)

// ScanAttributionRequest describes one late or normal scan to bind.
type ScanAttributionRequest struct {
	EventID    string
	Label      string
	Station    string
	OccurredAt time.Time
	// ContentScope is set only when the scan explicitly names the inner items
	// it covers; an empty scope after a parent split must NOT be expanded into
	// per-child confirmations.
	ContentScope []string
}

// ScanAttributionResult explains the binding decision with full basis.
type ScanAttributionResult struct {
	EventID          string   `json:"event_id"`
	Status           string   `json:"status"`
	ResolvedInstance string   `json:"resolved_instance_id,omitempty"`
	Match            string   `json:"match,omitempty"`
	Reason           string   `json:"reason"`
	EligibleTargets  []string `json:"eligible_targets,omitempty"`
	AliasTrail       []string `json:"alias_trail,omitempty"`
}

// AttributeScan binds a scan strictly to the instance valid when it occurred.
// A scan arriving after the parent was split, without an explicit content
// scope, stays unattributable: the platform never clones one confirmation into
// N child confirmations just because N children exist.
func (m *PedigreeModel) AttributeScan(r InstanceResolver, req ScanAttributionRequest) ScanAttributionResult {
	out := ScanAttributionResult{EventID: req.EventID, EligibleTargets: []string{}}
	res := r.Resolve(req.Label, req.Station, req.OccurredAt)
	out.Match, out.AliasTrail = res.Match, res.AliasTrail
	switch res.Match {
	case "active", "via_alias":
		out.Status = ScanAttributed
		out.ResolvedInstance = res.InstanceID
		out.Reason = "scan binds to the package instance effective at scan time"
		if res.Match == "via_alias" {
			out.Reason = res.Reason
		}
		// An explicit content scope must name instances that actually exist.
		if len(req.ContentScope) > 0 {
			for _, id := range req.ContentScope {
				if _, ok := m.Instances[id]; ok {
					out.EligibleTargets = append(out.EligibleTargets, id)
				}
			}
			if len(out.EligibleTargets) == 0 {
				out.Status = ScanUnattributable
				out.Reason = "declared content scope names no known instance; confirmation cannot be attached"
			}
		}
	case "retired":
		out.Status = ScanUnattributable
		out.ResolvedInstance = res.InstanceID
		out.Reason = "scan cites a package retired before scan time with no current alias; it must not be copied to split children"
	default:
		out.Status = ScanUnknownInstance
		out.Reason = res.Reason
	}
	// Post-split guard: even an active label that is a retired parent must not
	// fan out to children without an explicit scope.
	if out.Status == ScanAttributed && len(req.ContentScope) == 0 {
		if consumers := m.consumers[out.ResolvedInstance]; len(consumers) > 0 {
			// The instance itself was consumed by a later flow op before the
			// scan time: the scan covers no specific child content.
			for _, opKey := range consumers {
				if op := m.Operations[opKey]; op.OccurredAt.Before(req.OccurredAt) ||
					op.OccurredAt.Equal(req.OccurredAt) {
					if op.Kind == OpSplit || op.Kind == OpMixedRepack {
						out.Status = ScanUnattributable
						out.Reason = "parent package was already split/repacked at scan time and the scan names no content scope; not replicated to children"
						out.EligibleTargets = []string{}
						break
					}
				}
			}
		}
	}
	return out
}
