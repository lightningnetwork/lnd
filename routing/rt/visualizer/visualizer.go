// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package visualizer

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lightningnetwork/lnd/routing/rt/visualizer/prefix_tree"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"github.com/awalterschulze/gographviz"
	"encoding/hex"
)

type visualizer struct {
	// Graph used for visualisation.
	G                *graph.Graph
	// Vertexes which should be highlighted.
	HighlightedNodes []graph.Vertex
	// Edges which should be highlighted.
	HighlightedEdges []graph.Edge
	// Configuration parameters used for visualisation.
	Config           *VisualizerConfig
	// Function applied to node to obtain its label
	ApplyToNode      func(graph.Vertex) string
	// Function applied to edge to obtain its label
	ApplyToEdge      func(*graph.ChannelInfo) string
	// Prefix used for creating shortcuts.
	pt               prefix_tree.PrefixTree
	graphviz         *gographviz.Graph
}

// New creates new visualiser.
func New(g *graph.Graph, highlightedNodes []graph.Vertex,
	highlightedEdges []graph.Edge, config *VisualizerConfig) *visualizer {
	if config == nil {
		config = &DefaultVisualizerConfig
	}
	return &visualizer{
		G:                g,
		HighlightedNodes: highlightedNodes,
		HighlightedEdges: highlightedEdges,
		Config:           config,
		ApplyToNode:      func(v graph.Vertex) string { return hex.EncodeToString(v.ToByte()) },
		ApplyToEdge:      func(info *graph.ChannelInfo) string { return "nil" },
		pt:               prefix_tree.NewPrefixTree(),
		graphviz:         gographviz.NewGraph(),
	}
}

// EnableShortcut enables/disables shortcuts.
// Shortcut is a small unique string used for labeling.
func (viz *visualizer) EnableShortcut(value bool) {
	viz.Config.EnableShortcut = value
}

// BuildPrefixTree builds prefix tree for nodes.
// It is needed for shortcuts.
func (viz *visualizer) BuildPrefixTree() {
	for _, node := range viz.G.GetVertexes() {
		id := viz.ApplyToNode(node)
		viz.pt.Add(id)
	}
}

// Draw creates graph representation in Graphviz dot language.
func (viz *visualizer) Draw() string {
	viz.base()
	viz.drawNodes()
	viz.drawEdges()
	return viz.graphviz.String()
}

// Base makes initialization.
func (viz *visualizer) base() {
	viz.graphviz.SetName(viz.Config.GlobalCfg.Name)
	viz.graphviz.SetDir(viz.Config.GlobalCfg.Dir)
	viz.graphviz.SetStrict(viz.Config.GlobalCfg.Strict)
	// TODO(evg): use viz.Add(...) instead viz.Attrs.Add(...)
	viz.graphviz.Attrs.Add("bgcolor", viz.Config.GlobalCfg.BgColor)
}

func (viz *visualizer) drawNodes() {
	for _, node := range viz.G.GetVertexes() {
		if viz.isHighlightedNode(node) {
			viz.drawNode(node, viz.Config.HighlightedNodeCfg)
		} else {
			viz.drawNode(node, viz.Config.NodeCfg)
		}
	}
}

func (viz *visualizer) drawNode(node graph.Vertex, cfg *NodeConfig) {
	id := viz.ApplyToNode(node)
	if viz.Config.EnableShortcut {
		// TODO(evg): processing errors
		id, _ = viz.pt.Shortcut(id)
	}
	attrs := gographviz.Attrs{
		"shape":     cfg.Shape,
		"style":     cfg.Style,
		"fontsize":  cfg.FontSize,
		"fontcolor": cfg.FontColor,
		"color":     cfg.Color,
		"fillcolor": cfg.FillColor,
	}
	viz.graphviz.AddNode(viz.Config.GlobalCfg.Name, id, attrs)
}

func (viz *visualizer) isHighlightedNode(node graph.Vertex) bool {
	for _, value := range viz.HighlightedNodes {
		if node.String() == value.String() {
			return true
		}
	}
	return false
}

func (viz *visualizer) drawEdges() {
	for _, edge := range viz.G.GetUndirectedEdges() {
		if viz.isHighlightedEdge(edge) {
			viz.drawEdge(edge, viz.Config.HighlightedEdgeCfg)
		} else {
			viz.drawEdge(edge, viz.Config.EdgeCfg)
		}
	}
}

func (viz *visualizer) drawEdge(edge graph.Edge, cfg *EdgeConfig) {
	src := viz.ApplyToNode(edge.Src)
	tgt := viz.ApplyToNode(edge.Tgt)
	if viz.Config.EnableShortcut {
		// TODO(evg): processing errors
		src, _ = viz.pt.Shortcut(src)
		tgt, _ = viz.pt.Shortcut(tgt)
	}
	attrs := gographviz.Attrs{
		"fontsize":      cfg.FontSize,
		"fontcolor":     cfg.FontColor,
		"labeldistance": cfg.Scale,
		"dir":           cfg.Dir,
		"style":         cfg.Style,
		"color":         cfg.Color,
		"label":         viz.ApplyToEdge(edge.Info),
	}
	viz.graphviz.AddEdge(src, tgt, true, attrs)
}

func (viz *visualizer) isHighlightedEdge(edge graph.Edge) bool {
	for _, value := range viz.G.GetEdges() {
		if edge == value {
			return true
		}
	}
	return false
}

// Run graphviz command line utility (such as neato).
// Used for creation image from textual graph representation.
func Run(utility string, TempFile, ImageFile *os.File) error {
	extension := filepath.Ext(ImageFile.Name())[1:]
	_, err := exec.Command(utility, "-T"+extension, "-o"+ImageFile.Name(), TempFile.Name()).Output()
	return err
}

// Opens file in a command line open program.
// Used for displaying graphical files.
func Open(ImageFile *os.File) error {
	_, err := exec.Command("open", ImageFile.Name()).Output()
	return err
}
