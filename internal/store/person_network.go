package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	minPersonNetworkDepth = 1
	maxPersonNetworkDepth = 3
	maxPersonNetworkNodes = 250
	maxPersonNetworkEdges = 500
)

var ErrPersonNetworkInvalid = errors.New("invalid person network")

// PersonNetworkOptions bounds the curated relationship graph around one
// durable person root.
type PersonNetworkOptions struct {
	Depth        int
	IncludeEnded bool
}

// PersonNetwork is a bounded projection over declared person relationships
// and employment records. It never includes archive-derived associations.
type PersonNetwork struct {
	RootPersonID int64         `json:"root_person_id"`
	Depth        int           `json:"depth"`
	Truncated    bool          `json:"truncated"`
	Nodes        []NetworkNode `json:"nodes"`
	Edges        []NetworkEdge `json:"edges"`
}

// NetworkNode identifies a curated person or organization in a person
// network. ID is globally typed so person and organization IDs cannot collide.
type NetworkNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" enum:"person,organization"`
	EntityID int64  `json:"entity_id"`
	Label    string `json:"label"`
	Hop      int    `json:"hop"`
}

// NetworkEdge identifies a declared relationship or employment connection.
type NetworkEdge struct {
	ID                   string  `json:"id"`
	Kind                 string  `json:"kind" enum:"relationship,employment"`
	SourceNodeID         string  `json:"source_node_id"`
	TargetNodeID         string  `json:"target_node_id"`
	RelationshipTypeSlug *string `json:"relationship_type_slug,omitempty"`
	Label                string  `json:"label"`
	StartDate            *string `json:"start_date,omitempty"`
	EndDate              *string `json:"end_date,omitempty"`
}

// GetPersonNetworkContext returns a deterministic, breadth-first projection
// of one person's declared network. Only person_relationships and employments
// can introduce an edge; archive observations are intentionally excluded.
func (s *Store) GetPersonNetworkContext(
	ctx context.Context, personID int64, opts PersonNetworkOptions,
) (PersonNetwork, error) {
	if opts.Depth < minPersonNetworkDepth || opts.Depth > maxPersonNetworkDepth {
		return PersonNetwork{}, fmt.Errorf("%w: depth must be between %d and %d",
			ErrPersonNetworkInvalid, minPersonNetworkDepth, maxPersonNetworkDepth)
	}
	root, err := s.personNetworkPersonNode(ctx, personID, 0)
	if err != nil {
		return PersonNetwork{}, err
	}

	traversal := personNetworkTraversal{
		store: s,
		ctx:   ctx,
		opts:  opts,
		nodes: map[string]NetworkNode{root.ID: root},
		edges: make(map[string]NetworkEdge),
	}
	frontier := []NetworkNode{root}
	for hop := 0; hop < opts.Depth && len(frontier) > 0; hop++ {
		candidates, readErr := traversal.readLayer(frontier, hop+1)
		if readErr != nil {
			return PersonNetwork{}, readErr
		}
		frontier = traversal.admit(candidates)
		if traversal.truncated {
			break
		}
	}

	graph := PersonNetwork{
		RootPersonID: personID,
		Depth:        opts.Depth,
		Truncated:    traversal.truncated,
		Nodes:        make([]NetworkNode, 0, len(traversal.nodes)),
		Edges:        make([]NetworkEdge, 0, len(traversal.edges)),
	}
	for _, node := range traversal.nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	for _, edge := range traversal.edges {
		graph.Edges = append(graph.Edges, edge)
	}
	sortNetworkNodes(graph.Nodes)
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].Kind != graph.Edges[j].Kind {
			return graph.Edges[i].Kind < graph.Edges[j].Kind
		}
		if graph.Edges[i].Label != graph.Edges[j].Label {
			return graph.Edges[i].Label < graph.Edges[j].Label
		}
		return graph.Edges[i].ID < graph.Edges[j].ID
	})
	return graph, nil
}

type personNetworkTraversal struct {
	store     *Store
	ctx       context.Context
	opts      PersonNetworkOptions
	nodes     map[string]NetworkNode
	edges     map[string]NetworkEdge
	truncated bool
}

type personNetworkCandidate struct {
	node NetworkNode
	edge NetworkEdge
}

type personNetworkSourceEdge struct {
	frontier     NetworkNode
	edgeKind     string
	edgeEntityID int64
}

type personNetworkHydratedEdge struct {
	edge       NetworkEdge
	sourceNode NetworkNode
	targetNode NetworkNode
}

func (t *personNetworkTraversal) readLayer(
	frontier []NetworkNode, hop int,
) ([]personNetworkCandidate, error) {
	remaining := maxPersonNetworkEdges - len(t.edges)
	limit := remaining + 1
	sources, err := t.store.readPersonNetworkLayerSources(
		t.ctx, frontier, t.opts.IncludeEnded, limit)
	if err != nil {
		return nil, err
	}
	if hook := t.store.personNetworkSourceReadHook; hook != nil {
		hook(limit, len(sources))
	}
	if len(sources) > remaining {
		t.truncated = true
		sources = sources[:remaining]
	}
	unseen := make([]personNetworkSourceEdge, 0, len(sources))
	for _, source := range sources {
		if _, exists := t.edges[personNetworkEdgeID(source.edgeKind, source.edgeEntityID)]; !exists {
			unseen = append(unseen, source)
		}
	}
	return t.store.hydratePersonNetworkLayerCandidates(t.ctx, unseen, hop)
}

// admit consumes the already-deduplicated public-order prefix of the bounded
// source subset. Stopping at the first node or edge omission keeps admission
// deterministic even when the source-work bound underfills the output caps.
func (t *personNetworkTraversal) admit(candidates []personNetworkCandidate) []NetworkNode {
	next := make([]NetworkNode, 0)
	for _, candidate := range candidates {
		if _, exists := t.edges[candidate.edge.ID]; exists {
			continue
		}
		_, nodeExists := t.nodes[candidate.node.ID]
		if !nodeExists && len(t.nodes) >= maxPersonNetworkNodes {
			t.truncated = true
			break
		}
		if len(t.edges) >= maxPersonNetworkEdges {
			t.truncated = true
			break
		}
		t.edges[candidate.edge.ID] = candidate.edge
		if nodeExists {
			continue
		}
		t.nodes[candidate.node.ID] = candidate.node
		next = append(next, candidate.node)
	}
	return next
}

func (s *Store) readPersonNetworkLayerSources(
	ctx context.Context, frontier []NetworkNode, includeEnded bool, limit int,
) ([]personNetworkSourceEdge, error) {
	orderedFrontier := append([]NetworkNode(nil), frontier...)
	sortNetworkNodes(orderedFrontier)
	sources := make([]personNetworkSourceEdge, 0, limit)
	appendIDs := func(frontierNode NetworkNode, edgeKind string, ids []int64) {
		for _, id := range ids {
			sources = append(sources, personNetworkSourceEdge{
				frontier: frontierNode, edgeKind: edgeKind, edgeEntityID: id,
			})
		}
	}
	for _, node := range orderedFrontier {
		remaining := limit - len(sources)
		if remaining <= 0 {
			break
		}
		switch node.Kind {
		case "person":
			ids, err := s.readPersonNetworkEmploymentAdjacencyIDs(
				ctx, "person_id", node.EntityID, includeEnded, remaining)
			if err != nil {
				return nil, err
			}
			appendIDs(node, "employment", ids)
			remaining = limit - len(sources)
			if remaining <= 0 {
				break
			}
			ids, err = s.readPersonNetworkRelationshipAdjacencyIDs(
				ctx, node.EntityID, includeEnded, remaining)
			if err != nil {
				return nil, err
			}
			appendIDs(node, "relationship", ids)
		case "organization":
			ids, err := s.readPersonNetworkEmploymentAdjacencyIDs(
				ctx, "organization_id", node.EntityID, includeEnded, remaining)
			if err != nil {
				return nil, err
			}
			appendIDs(node, "employment", ids)
		default:
			return nil, fmt.Errorf("%w: unknown node kind %q", ErrPersonNetworkInvalid, node.Kind)
		}
	}
	return sources, nil
}

func (s *Store) readPersonNetworkEmploymentAdjacencyIDs(
	ctx context.Context, column string, entityID int64, includeEnded bool, limit int,
) ([]int64, error) {
	query := s.personNetworkEmploymentAdjacencyQuery(column, includeEnded)
	return s.readPersonNetworkAdjacencyIDs(ctx, query, []any{entityID, limit}, "employment")
}

func (s *Store) personNetworkEmploymentAdjacencyQuery(column string, includeEnded bool) string {
	current := ""
	if !includeEnded {
		current = " AND " + s.dialect.BoolTrueExpr("is_current")
	}
	return `SELECT id FROM employments WHERE ` + column + ` = ?` + current + ` ORDER BY id LIMIT ?`
}

func (s *Store) readPersonNetworkRelationshipAdjacencyIDs(
	ctx context.Context, personID int64, includeEnded bool, limit int,
) ([]int64, error) {
	return s.readPersonNetworkAdjacencyIDs(ctx,
		personNetworkRelationshipAdjacencyQuery(includeEnded),
		[]any{personID, personID, limit}, "relationship")
}

func personNetworkRelationshipAdjacencyQuery(includeEnded bool) string {
	current := ""
	if !includeEnded {
		current = " AND end_year IS NULL"
	}
	return `SELECT id FROM person_relationships WHERE source_person_id = ?` + current + `
	UNION ALL
	SELECT id FROM person_relationships WHERE target_person_id = ?` + current + `
	ORDER BY id LIMIT ?`
}

func (s *Store) readPersonNetworkAdjacencyIDs(
	ctx context.Context, query string, args []any, edgeKind string,
) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read person network %s adjacency: %w", edgeKind, err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan person network %s adjacency: %w", edgeKind, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read person network %s adjacency: %w", edgeKind, err)
	}
	return ids, nil
}

func (s *Store) hydratePersonNetworkLayerCandidates(
	ctx context.Context, sources []personNetworkSourceEdge, hop int,
) ([]personNetworkCandidate, error) {
	relationshipIDs := make(map[int64]struct{})
	employmentIDs := make(map[int64]struct{})
	for _, source := range sources {
		switch source.edgeKind {
		case "relationship":
			relationshipIDs[source.edgeEntityID] = struct{}{}
		case "employment":
			employmentIDs[source.edgeEntityID] = struct{}{}
		default:
			return nil, fmt.Errorf("%w: unknown edge kind %q", ErrPersonNetworkInvalid, source.edgeKind)
		}
	}
	hydrated := make(map[string]personNetworkHydratedEdge, len(relationshipIDs)+len(employmentIDs))
	if len(relationshipIDs) > 0 {
		edges, err := s.hydratePersonNetworkRelationships(ctx, personNetworkSetIDs(relationshipIDs))
		if err != nil {
			return nil, err
		}
		for id, edge := range edges {
			hydrated[personNetworkEdgeID("relationship", id)] = edge
		}
	}
	if len(employmentIDs) > 0 {
		edges, err := s.hydratePersonNetworkEmployments(ctx, personNetworkSetIDs(employmentIDs))
		if err != nil {
			return nil, err
		}
		for id, edge := range edges {
			hydrated[personNetworkEdgeID("employment", id)] = edge
		}
	}
	unique := make(map[string]personNetworkCandidate, len(hydrated))
	for _, source := range sources {
		key := personNetworkEdgeID(source.edgeKind, source.edgeEntityID)
		edge, exists := hydrated[key]
		if !exists {
			return nil, fmt.Errorf("hydrate person network edge %s: missing row", key)
		}
		candidate, err := edge.candidateFrom(source.frontier, hop)
		if err != nil {
			return nil, err
		}
		previous, exists := unique[key]
		if !exists || lessPersonNetworkCandidate(candidate, previous) {
			unique[key] = candidate
		}
	}
	candidates := make([]personNetworkCandidate, 0, len(unique))
	for _, candidate := range unique {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return lessPersonNetworkCandidate(candidates[i], candidates[j])
	})
	return candidates, nil
}

func (s *Store) hydratePersonNetworkRelationships(
	ctx context.Context, ids []int64,
) (map[int64]personNetworkHydratedEdge, error) {
	cte, args := personNetworkIDValuesCTE("selected_relationships", ids)
	rows, err := s.db.QueryContext(ctx, `
		WITH `+cte+`
		SELECT relationship.id,
		       relationship.source_person_id,
		       COALESCE(NULLIF(source_person.display_name, ''), source_person.vcard_uid),
		       relationship.target_person_id,
		       COALESCE(NULLIF(target_person.display_name, ''), target_person.vcard_uid),
		       relationship_type.slug,
		       relationship_type.forward_label,
		       relationship.start_year, relationship.start_month, relationship.start_day,
		       relationship.end_year, relationship.end_month, relationship.end_day
		FROM selected_relationships selected
		JOIN person_relationships relationship ON relationship.id = selected.id
		JOIN relationship_types relationship_type ON relationship_type.id = relationship.relationship_type_id
		JOIN persons source_person ON source_person.id = relationship.source_person_id
		JOIN persons target_person ON target_person.id = relationship.target_person_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrate person network relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hydrated := make(map[int64]personNetworkHydratedEdge, len(ids))
	for rows.Next() {
		var (
			id, sourceID, targetID                int64
			sourceLabel, targetLabel, slug, label string
			startYear, startMonth, startDay       sql.NullInt64
			endYear, endMonth, endDay             sql.NullInt64
		)
		if err := rows.Scan(
			&id, &sourceID, &sourceLabel, &targetID, &targetLabel, &slug, &label,
			&startYear, &startMonth, &startDay, &endYear, &endMonth, &endDay,
		); err != nil {
			return nil, fmt.Errorf("scan person network relationship: %w", err)
		}
		relationshipSlug := slug
		hydrated[id] = personNetworkHydratedEdge{
			edge: NetworkEdge{
				ID:                   personNetworkEdgeID("relationship", id),
				Kind:                 "relationship",
				SourceNodeID:         personNetworkNodeID("person", sourceID),
				TargetNodeID:         personNetworkNodeID("person", targetID),
				RelationshipTypeSlug: &relationshipSlug,
				Label:                label,
				StartDate:            personNetworkDateFromColumns(startYear, startMonth, startDay),
				EndDate:              personNetworkDateFromColumns(endYear, endMonth, endDay),
			},
			sourceNode: NetworkNode{
				ID: personNetworkNodeID("person", sourceID), Kind: "person", EntityID: sourceID, Label: sourceLabel,
			},
			targetNode: NetworkNode{
				ID: personNetworkNodeID("person", targetID), Kind: "person", EntityID: targetID, Label: targetLabel,
			},
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate person network relationships: %w", err)
	}
	return hydrated, nil
}

func (s *Store) hydratePersonNetworkEmployments(
	ctx context.Context, ids []int64,
) (map[int64]personNetworkHydratedEdge, error) {
	cte, args := personNetworkIDValuesCTE("selected_employments", ids)
	rows, err := s.db.QueryContext(ctx, `
		WITH `+cte+`
		SELECT employment.id,
		       employment.person_id,
		       COALESCE(NULLIF(person.display_name, ''), person.vcard_uid),
		       employment.organization_id,
		       organization.name,
		       COALESCE(NULLIF(employment.title, ''), NULLIF(employment.role, ''), 'employment'),
		       employment.start_year, employment.start_month, employment.start_day,
		       employment.end_year, employment.end_month, employment.end_day
		FROM selected_employments selected
		JOIN employments employment ON employment.id = selected.id
		JOIN persons person ON person.id = employment.person_id
		JOIN organizations organization ON organization.id = employment.organization_id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrate person network employments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hydrated := make(map[int64]personNetworkHydratedEdge, len(ids))
	for rows.Next() {
		var (
			id, personID, organizationID              int64
			personLabel, organizationLabel, edgeLabel string
			startYear, startMonth, startDay           sql.NullInt64
			endYear, endMonth, endDay                 sql.NullInt64
		)
		if err := rows.Scan(
			&id, &personID, &personLabel, &organizationID, &organizationLabel, &edgeLabel,
			&startYear, &startMonth, &startDay, &endYear, &endMonth, &endDay,
		); err != nil {
			return nil, fmt.Errorf("scan person network employment: %w", err)
		}
		hydrated[id] = personNetworkHydratedEdge{
			edge: NetworkEdge{
				ID:           personNetworkEdgeID("employment", id),
				Kind:         "employment",
				SourceNodeID: personNetworkNodeID("person", personID),
				TargetNodeID: personNetworkNodeID("organization", organizationID),
				Label:        edgeLabel,
				StartDate:    personNetworkDateFromColumns(startYear, startMonth, startDay),
				EndDate:      personNetworkDateFromColumns(endYear, endMonth, endDay),
			},
			sourceNode: NetworkNode{
				ID: personNetworkNodeID("person", personID), Kind: "person", EntityID: personID, Label: personLabel,
			},
			targetNode: NetworkNode{
				ID: personNetworkNodeID("organization", organizationID), Kind: "organization", EntityID: organizationID, Label: organizationLabel,
			},
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate person network employments: %w", err)
	}
	return hydrated, nil
}

func (edge personNetworkHydratedEdge) candidateFrom(
	frontier NetworkNode, hop int,
) (personNetworkCandidate, error) {
	var node NetworkNode
	switch frontier.ID {
	case edge.sourceNode.ID:
		node = edge.targetNode
	case edge.targetNode.ID:
		node = edge.sourceNode
	default:
		return personNetworkCandidate{}, fmt.Errorf(
			"%w: edge %s is not adjacent to frontier node %s",
			ErrPersonNetworkInvalid, edge.edge.ID, frontier.ID,
		)
	}
	node.Hop = hop
	return personNetworkCandidate{node: node, edge: edge.edge}, nil
}

func lessPersonNetworkCandidate(left, right personNetworkCandidate) bool {
	if left.node.Hop != right.node.Hop {
		return left.node.Hop < right.node.Hop
	}
	if left.node.Kind != right.node.Kind {
		return left.node.Kind < right.node.Kind
	}
	if left.node.Label != right.node.Label {
		return left.node.Label < right.node.Label
	}
	if left.node.ID != right.node.ID {
		return left.node.ID < right.node.ID
	}
	if left.edge.Kind != right.edge.Kind {
		return left.edge.Kind < right.edge.Kind
	}
	return left.edge.ID < right.edge.ID
}

func personNetworkEdgeID(kind string, entityID int64) string {
	return fmt.Sprintf("%s:%d", kind, entityID)
}

func personNetworkSetIDs(set map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

func personNetworkIDValuesCTE(name string, ids []int64) (string, []any) {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	values := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		values[index] = "(CAST(? AS BIGINT))"
		args[index] = id
	}
	return name + "(id) AS (VALUES " + strings.Join(values, ", ") + ")", args
}

func personNetworkDateFromColumns(year, month, day sql.NullInt64) *string {
	date := ScanPartialDate(year, month, day)
	if date.IsZero() {
		return nil
	}
	return personNetworkDate(&date)
}

func (s *Store) personNetworkPersonNode(ctx context.Context, personID int64, hop int) (NetworkNode, error) {
	var (
		id          int64
		displayName sql.NullString
		vcardUID    string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, vcard_uid
		FROM persons
		WHERE id = ?
	`, personID).Scan(&id, &displayName, &vcardUID)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkNode{}, ErrPersonNotFound
	}
	if err != nil {
		return NetworkNode{}, fmt.Errorf("get person network node %d: %w", personID, err)
	}
	label := vcardUID
	if displayName.Valid && displayName.String != "" {
		label = displayName.String
	}
	return NetworkNode{ID: personNetworkNodeID("person", id), Kind: "person", EntityID: id, Label: label, Hop: hop}, nil
}

func personNetworkNodeID(kind string, entityID int64) string {
	return fmt.Sprintf("%s:%d", kind, entityID)
}

func personNetworkDate(date *PartialDate) *string {
	if date == nil {
		return nil
	}
	value := date.String()
	return &value
}

func sortNetworkNodes(nodes []NetworkNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hop != nodes[j].Hop {
			return nodes[i].Hop < nodes[j].Hop
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		if nodes[i].Label != nodes[j].Label {
			return nodes[i].Label < nodes[j].Label
		}
		return nodes[i].ID < nodes[j].ID
	})
}
