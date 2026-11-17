package domain

import (
	"fmt"
)

// Bounded integer flow engine.
//
// The pedigree problem is a lower-bounded supply/demand flow on a DAG of
// packaging operations: explicit allocations are edge lower bounds, package
// quantities are edge/node capacities, original sources are supply nodes and
// surviving packages are demand nodes. Capacities are integers and the
// constraint matrix is network-structured (totally unimodular), so integral
// max-flow yields exact integer bounds — no fractional "half an item" answer
// is ever produced.
//
// Solve steps (all auditable):
//  1. the standard super-source/super-sink transform finds ONE feasible
//     b-flow that routes the ENTIRE supply through every constraint — this is
//     the complete chain-wide witness, never a single-edge clip;
//  2. flow decomposition on the DAG reads the current source->terminal flow;
//  3. residual max-flow gives the absolute maximum amount that can reach a
//     target (current flow + reroute capacity);
//  4. the minimum follows by complement: the least flow into target t is the
//     source total minus the most that can reach every other terminal.
//
// Edmonds-Karp keeps the implementation small and deterministic; callers bound
// augmentations and turn a resource ceiling into an explicit unverified
// result instead of presenting a partial allocation as a boundary.

// EdgeMeta labels an arc so witness allocations are explainable.
type EdgeMeta struct {
	OpKey  string `json:"op_key,omitempty"`
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"` // allocation|carry|boundary
	Unit   string `json:"unit,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
}

type flowArc struct {
	from, to int
	residual int64 // remaining capacity (initial capacity minus current flow)
	lower    int64
	cap      int64
	rev      int
	meta     EdgeMeta
	orig     bool
}

// FlowNetwork is a directed multigraph with integral lower bounds.
type FlowNetwork struct {
	adj      [][]flowArc
	totalQty int64
}

// NewFlowNetwork sizes the graph for n vertices.
func NewFlowNetwork(n int, totalQty int64) *FlowNetwork {
	if totalQty <= 0 {
		totalQty = 1
	}
	return &FlowNetwork{adj: make([][]flowArc, n), totalQty: totalQty}
}

// TotalQty is the safe "infinite" stand-in.
func (f *FlowNetwork) TotalQty() int64 { return f.totalQty }

// AddEdge inserts an original arc with lower<=flow<=cap.
func (f *FlowNetwork) AddEdge(from, to int, lower, cap int64, m EdgeMeta) error {
	if from < 0 || to < 0 || from >= len(f.adj) || to >= len(f.adj) {
		return fmt.Errorf("flow edge vertex out of range: %d->%d", from, to)
	}
	if lower < 0 || cap < lower {
		return fmt.Errorf("flow edge %s->%s has invalid bounds [%d,%d]", m.From, m.To, lower, cap)
	}
	f.adj[from] = append(f.adj[from], flowArc{
		from: from, to: to, residual: cap - lower, lower: lower, cap: cap,
		rev: len(f.adj[to]), meta: m, orig: true,
	})
	f.adj[to] = append(f.adj[to], flowArc{
		from: to, to: from, residual: 0, cap: 0, rev: len(f.adj[from]) - 1,
		// Reverse residual arcs inherit the original meta (with a reverse
		// marker) so a filtered reroute can tell internal edges apart from
		// boundary/circulation/helper arcs.
		meta: EdgeMeta{OpKey: m.OpKey, From: m.To, To: m.From, Kind: m.Kind, Unit: m.Unit, Pinned: m.Pinned},
	})
	f.adj[from][len(f.adj[from])-1].rev = len(f.adj[to]) - 1
	return nil
}

// Vertices returns the vertex count.
func (f *FlowNetwork) Vertices() int { return len(f.adj) }

// AddVertex appends an isolated vertex and returns its id. Cloned networks use
// it to attach forced-measurement edges without disturbing existing arcs.
func (f *FlowNetwork) AddVertex() int {
	f.adj = append(f.adj, nil)
	return len(f.adj) - 1
}

// maxFlow runs Edmonds-Karp; returns augmented amount and limit-hit flag.
func (f *FlowNetwork) maxFlow(s, t, augmentLimit int) (int64, bool) {
	return f.maxFlowFiltered(s, t, augmentLimit, false)
}

// maxFlowOriginal is like maxFlow but never traverses a feasibility helper
// (or its reverse), so a lower-bound cancellation reroutes only through the
// reverse residual arcs of original edges and cannot take an artificial
// shortcut through SS/TT.
func (f *FlowNetwork) maxFlowOriginal(s, t, augmentLimit int) (int64, bool) {
	return f.maxFlowFiltered(s, t, augmentLimit, true)
}

// maxFlowFiltered runs Edmonds-Karp, excluding helper-kind arcs when
// skipHelpers is set. Original-edge reverse residual arcs carry no helper
// marker and remain available for cancellation.
func (f *FlowNetwork) maxFlowFiltered(s, t, augmentLimit int, skipHelpers bool) (int64, bool) {
	var total int64
	augments := 0
	for {
		if augmentLimit > 0 && augments >= augmentLimit {
			return total, true
		}
		parentV := make([]int, len(f.adj))
		parentA := make([]int, len(f.adj))
		for i := range parentV {
			parentV[i] = -1
		}
		parentV[s] = s
		queue := []int{s}
		for len(queue) > 0 && parentV[t] == -1 {
			u := queue[0]
			queue = queue[1:]
			for i, a := range f.adj[u] {
				// Only feasibility-helper arcs are excluded; reverse residual
				// arcs of original edges have an empty/non-helper kind and
				// must stay available for cancellation.
				if skipHelpers && a.meta.Kind == "helper" {
					continue
				}
				if a.residual > 0 && parentV[a.to] == -1 {
					parentV[a.to] = u
					parentA[a.to] = i
					queue = append(queue, a.to)
					if a.to == t {
						break
					}
				}
			}
		}
		if parentV[t] == -1 {
			return total, false
		}
		bottleneck := int64(-1)
		for v := t; v != s; v = parentV[v] {
			a := f.adj[parentV[v]][parentA[v]]
			if bottleneck < 0 || a.residual < bottleneck {
				bottleneck = a.residual
			}
		}
		if bottleneck <= 0 {
			return total, false
		}
		for v := t; v != s; v = parentV[v] {
			u := parentV[v]
			i := parentA[v]
			a := &f.adj[u][i]
			a.residual -= bottleneck
			f.adj[v][a.rev].residual += bottleneck
		}
		total += bottleneck
		augments++
	}
}

// WitnessFlow is one original arc's flow in the placed feasible allocation.
type WitnessFlow struct {
	Meta EdgeMeta `json:"meta"`
	Flow int64    `json:"flow"`
}

// FlowBounds is the exact integral feasible range for one (source, target).
type FlowBounds struct {
	Lower           int64         `json:"lower"`
	Upper           int64         `json:"upper"`
	Status          string        `json:"status"` // exact|partial|uncertain|absent|infeasible|unverified
	Witness         []WitnessFlow `json:"witness_allocation,omitempty"`
	ResourceLimit   bool          `json:"resource_limit_reached,omitempty"`
	InfeasibleNotes []string      `json:"infeasible_notes,omitempty"`
}

func (f *FlowNetwork) edgeFlow(a flowArc) int64 {
	// residual was initialised to cap-lower; used residual capacity equals the
	// flow added beyond the lower bound, so total flow is cap-residual.
	return a.cap - a.residual
}

func (f *FlowNetwork) witness() []WitnessFlow {
	rows := make([]WitnessFlow, 0)
	for u := range f.adj {
		for _, a := range f.adj[u] {
			if !a.orig || a.meta.Kind == "pair_return" || a.meta.Kind == "circulation" || a.meta.Kind == "helper" {
				continue
			}
			rows = append(rows, WitnessFlow{Meta: a.meta, Flow: f.edgeFlow(a)})
		}
	}
	return rows
}

// helperArc locates a feasibility helper so it can be frozen afterwards.
type helperArc struct {
	from, to, index int
}

// Outflow sums current original-arc flow leaving v.
func (f *FlowNetwork) Outflow(v int) int64 {
	var sum int64
	for _, a := range f.adj[v] {
		if a.orig {
			sum += f.edgeFlow(a)
		}
	}
	return sum
}

// MinMaxPairFlow returns the exact integral min/max flow from origin vertex s
// to terminal vertex t under node supplies/demands and edge lower bounds.
//
// It identifies every edge leaving s that can reach t (in the layered pedigree
// graph their total flow is exactly the s->t pair flow, including across
// multi-hop chains) and binary-searches feasible bounds by overriding those
// edges, re-running the full SS/TT b-flow feasibility transform for every
// candidate:
//
//   - upper: largest k for which forcing the pair edges to lower=k is still
//     feasible (an allocation with >=k units s->t exists);
//   - lower: smallest k for which capping the pair edges at cap=k keeps the
//     whole network feasible (an allocation with <=k units s->t exists).
//
// Each oracle call routes the ENTIRE supply through every conservation
// constraint, so every boundary has a complete chain-wide feasible allocation
// behind it. Integral capacities on a totally unimodular network give exact
// non-negative integers; a fractional half-item answer is impossible.
func (f *FlowNetwork) MinMaxPairFlow(s, t int, supplies, demands map[int]int64, augmentLimit int) FlowBounds {
	if s == t {
		return FlowBounds{Status: "infeasible", InfeasibleNotes: []string{"source and target resolve to the same vertex"}}
	}
	var totalSupply, totalDemand, sourceSupply int64
	for v, q := range supplies {
		totalSupply += q
		if v == s {
			sourceSupply = q
		}
	}
	for _, q := range demands {
		totalDemand += q
	}
	if totalSupply != totalDemand || totalSupply <= 0 || sourceSupply <= 0 {
		return FlowBounds{Status: "infeasible", InfeasibleNotes: []string{
			fmt.Sprintf("supply %d and demand %d must be equal and positive", totalSupply, totalDemand)}}
	}
	// Every original edge on an s->t path. In a layered DAG the flow across any
	// such cut equals the s->t pair flow, so capping/forcing all of them bounds
	// the pair flow even across multi-hop chains.
	canReachT := f.reaches(t)
	reachableFromS := f.reachableFrom(s)
	type edgeRef struct {
		from, idx  int
		lower, cap int64
	}
	var pairEdges []edgeRef
	for u := range f.adj {
		if !reachableFromS[u] {
			continue
		}
		for i, a := range f.adj[u] {
			if a.orig && canReachT[a.to] && reachableFromS[a.to] {
				pairEdges = append(pairEdges, edgeRef{from: u, idx: i, lower: a.lower, cap: a.cap})
			}
		}
	}
	if len(pairEdges) == 0 {
		return FlowBounds{Status: "absent", Lower: 0, Upper: 0}
	}
	// oracle overrides every s->t path edge and re-checks whole-network
	// feasibility. Forcing lower=k means at least k units travel s->t; capping
	// cap=k means no more than k can.
	oracle := func(k int64, forceLower bool) (bool, bool, []WitnessFlow) {
		net := f.Clone()
		for _, ref := range pairEdges {
			a := &net.adj[ref.from][ref.idx]
			if forceLower {
				a.lower = k
				if a.cap < k {
					a.cap = k
				}
			} else {
				a.lower = 0
				a.cap = k
			}
			a.residual = a.cap - a.lower
		}
		n := len(net.adj)
		r := make([]int64, n)
		for u := 0; u < n; u++ {
			for _, a := range net.adj[u] {
				if !a.orig {
					continue
				}
				r[u] -= a.lower
				r[a.to] += a.lower
			}
		}
		for v, q := range supplies {
			r[v] += q
		}
		for v, q := range demands {
			r[v] -= q
		}
		ss, tt := n, n+1
		net.adj = append(net.adj, nil, nil)
		var required int64
		addHelper := func(from, to int, capacity int64) {
			if capacity == 0 {
				return
			}
			idx := len(net.adj[from])
			net.adj[from] = append(net.adj[from], flowArc{
				from: from, to: to, residual: capacity, cap: capacity,
				rev: len(net.adj[to]), orig: false, meta: EdgeMeta{Kind: "helper"},
			})
			net.adj[to] = append(net.adj[to], flowArc{
				from: to, to: from, residual: 0, cap: 0,
				rev: idx, orig: false, meta: EdgeMeta{Kind: "helper"},
			})
			net.adj[from][idx].rev = len(net.adj[to]) - 1
		}
		for v := 0; v < n; v++ {
			switch {
			case r[v] > 0:
				addHelper(ss, v, r[v])
				required += r[v]
			case r[v] < 0:
				addHelper(v, tt, -r[v])
			}
		}
		got, limited := net.maxFlow(ss, tt, augmentLimit)
		if limited && got < required {
			return false, true, nil
		}
		if got != required {
			return false, false, nil
		}
		return true, false, net.witness()
	}

	// Upper: largest feasible forced lower k.
	var upper int64
	var witness []WitnessFlow
	lo, hi := int64(0), sourceSupply
	for lo <= hi {
		mid := lo + (hi-lo)/2
		ok, limited, w := oracle(mid, true)
		if limited {
			return FlowBounds{Status: "unverified", ResourceLimit: true,
				InfeasibleNotes: []string{"resource limit reached while computing the upper bound"}}
		}
		if ok {
			upper = mid
			witness = w
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	// Lower: smallest feasible cap k.
	lower := sourceSupply
	lo, hi = 0, sourceSupply
	for lo <= hi {
		mid := lo + (hi-lo)/2
		ok, limited, _ := oracle(mid, false)
		if limited {
			return FlowBounds{Status: "unverified", ResourceLimit: true,
				InfeasibleNotes: []string{"resource limit reached while computing the lower bound"}}
		}
		if ok {
			lower = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	if lower > upper {
		lower = upper
	}
	res := FlowBounds{Lower: lower, Upper: upper, Witness: witness}
	switch {
	case lower == upper && upper > 0:
		res.Status = "exact"
	case upper == 0:
		res.Status = "absent"
	case lower == 0:
		res.Status = "uncertain"
	default:
		res.Status = "partial"
	}
	return res
}

// reaches returns the set of vertices from which target is reachable over
// original edges (reverse reachability).
func (f *FlowNetwork) reaches(target int) map[int]bool {
	rev := map[int][]int{}
	for u := range f.adj {
		for _, a := range f.adj[u] {
			if a.orig {
				rev[a.to] = append(rev[a.to], u)
			}
		}
	}
	seen := map[int]bool{target: true}
	queue := []int{target}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, u := range rev[v] {
			if !seen[u] {
				seen[u] = true
				queue = append(queue, u)
			}
		}
	}
	return seen
}

// reachableFrom returns every vertex reachable from source over original
// edges (forward reachability).
func (f *FlowNetwork) reachableFrom(source int) map[int]bool {
	seen := map[int]bool{source: true}
	queue := []int{source}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, a := range f.adj[v] {
			if a.orig && !seen[a.to] {
				seen[a.to] = true
				queue = append(queue, a.to)
			}
		}
	}
	return seen
}

// PairFlow decomposes the placed feasible b-flow and sums the flow travelling
// from origin s to terminal t over forward DAG edges, deterministically.
func (f *FlowNetwork) PairFlow(s, t int) int64 {
	type key struct{ u, idx int }
	remaining := map[key]int64{}
	for u := range f.adj {
		for i, a := range f.adj[u] {
			if a.orig && a.meta.Kind != "pair_return" && a.meta.Kind != "helper" {
				remaining[key{u, i}] = f.capMinusResidual(a)
			}
		}
	}
	var total int64
	for {
		parentV := map[int]int{}
		parentA := map[int]int{}
		parentV[s] = s
		stack := []int{s}
		found := false
		for len(stack) > 0 && !found {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for i, a := range f.adj[u] {
				if !a.orig || a.meta.Kind == "pair_return" || a.meta.Kind == "helper" || remaining[key{u, i}] <= 0 {
					continue
				}
				if _, seen := parentV[a.to]; seen {
					continue
				}
				parentV[a.to] = u
				parentA[a.to] = i
				if a.to == t {
					found = true
					break
				}
				stack = append(stack, a.to)
			}
		}
		if !found {
			return total
		}
		bottleneck := int64(-1)
		for v := t; v != s; v = parentV[v] {
			u, i := parentV[v], parentA[v]
			r := remaining[key{u, i}]
			if bottleneck < 0 || r < bottleneck {
				bottleneck = r
			}
		}
		if bottleneck <= 0 {
			return total
		}
		for v := t; v != s; v = parentV[v] {
			u, i := parentV[v], parentA[v]
			remaining[key{u, i}] -= bottleneck
		}
		total += bottleneck
	}
}

// capMinusResidual is the current integral flow on an arc (residual was
// initialised to cap-lower, so cap-residual equals lower + used residual).
func (f *FlowNetwork) capMinusResidual(a flowArc) int64 { return a.cap - a.residual }

// Clone returns a deep copy so repeated bound queries on the same feasible
// allocation do not contaminate each other's residual reroutes.
func (f *FlowNetwork) Clone() *FlowNetwork {
	cp := &FlowNetwork{
		adj:      make([][]flowArc, len(f.adj)),
		totalQty: f.totalQty,
	}
	for i := range f.adj {
		cp.adj[i] = append([]flowArc(nil), f.adj[i]...)
	}
	return cp
}
