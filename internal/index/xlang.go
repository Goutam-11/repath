package index

import (
	"context"
	"fmt"
	"sort"

	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/store"
)

// Cross-language links are derived, persisted edges: a rule-tagged REFERENCES
// row at xlangConfidence (below the default 0.5 query floor), rebuilt from the
// persisted anchors on every run that writes files. No new edge kind and no
// resolver change: impact, path, and map walk the edges table as usual.
const (
	xlangRulePrefix   = "xlang."
	xlangConfidence   = 0.45
	xlangMethodAll    = "ALL"
	xlangEdgeKind     = "REFERENCES"
	xlangHTTPProtocol = "http"
)

func xlangRule(protocol string) string { return xlangRulePrefix + protocol + ".route" }

// toAnchorRows resolves extracted anchors to node ids in the file being
// written; an anchor whose enclosing symbol was not extracted is dropped.
func toAnchorRows(anchors []facts.Anchor, ids map[string]int64, lang string) []store.AnchorRow {
	rows := make([]store.AnchorRow, 0, len(anchors))
	for _, anchor := range anchors {
		nodeID, ok := ids[anchor.QName]
		if !ok {
			continue
		}
		if anchor.Protocol == "" || anchor.Role == "" || anchor.Path == "" {
			continue
		}
		rows = append(rows, store.AnchorRow{
			NodeID: nodeID, Lang: lang, Protocol: anchor.Protocol, Role: anchor.Role,
			Method: anchor.Method, Path: anchor.Path, Line: anchor.Line,
			Confidence: anchor.Confidence,
		})
	}
	return rows
}

// linkCrossLanguage rebuilds every cross-language edge from the persisted
// anchors. It deletes only its own rule-prefixed rows first, then matches each
// client anchor to every server anchor with the same protocol and path (and the
// same method, or a server "ALL" wildcard) in a different language. Running the
// same rebuild on cold, incremental, and full indexes is what keeps their edge
// row sets identical (#49).
func linkCrossLanguage(ctx context.Context, st *store.Store) error {
	if _, err := st.DeleteEdgesByRulePrefix(ctx, xlangRulePrefix); err != nil {
		return err
	}
	anchors, err := st.Anchors(ctx)
	if err != nil {
		return err
	}
	if len(anchors) == 0 {
		return nil
	}

	type matchKey struct {
		protocol string
		method   string
		path     string
	}
	servers := make(map[matchKey][]store.AnchorRow)
	for _, anchor := range anchors {
		if anchor.Role != "server" {
			continue
		}
		key := matchKey{protocol: anchor.Protocol, method: anchor.Method, path: anchor.Path}
		servers[key] = append(servers[key], anchor)
	}

	keys := make([]matchKey, 0, len(servers))
	for key := range servers {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].protocol != keys[j].protocol {
			return keys[i].protocol < keys[j].protocol
		}
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].path < keys[j].path
	})

	var edges []store.Edge
	seen := make(map[string]bool)
	for _, client := range anchors {
		if client.Role != "client" {
			continue
		}
		candidates := append([]store.AnchorRow{}, servers[matchKey{
			protocol: client.Protocol, method: client.Method, path: client.Path,
		}]...)
		if client.Method != xlangMethodAll {
			candidates = append(candidates, servers[matchKey{
				protocol: client.Protocol, method: xlangMethodAll, path: client.Path,
			}]...)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].NodeID < candidates[j].NodeID })
		for _, server := range candidates {
			if server.NodeID == client.NodeID || server.Lang == client.Lang {
				continue
			}
			edgeKey := fmt.Sprintf("%d/%d", client.NodeID, server.NodeID)
			if seen[edgeKey] {
				continue
			}
			seen[edgeKey] = true
			edges = append(edges, store.Edge{
				Src: client.NodeID, Dst: server.NodeID, Kind: xlangEdgeKind,
				Confidence: xlangConfidence, Rule: xlangRule(client.Protocol), Line: client.Line,
			})
		}
	}
	if len(edges) == 0 {
		return nil
	}
	_, _, err = st.MergeEdges(ctx, edges)
	return err
}
