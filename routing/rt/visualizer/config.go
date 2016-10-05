// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package visualizer

// Configuration used for graph visualisation.
type VisualizerConfig struct {
	// General options.
	GlobalCfg          *GlobalConfig
	// Options for node visualisation.
	NodeCfg            *NodeConfig
	// Options ofr highlighted nodes visualisation.
	HighlightedNodeCfg *NodeConfig
	// Options for edges visualisation.
	EdgeCfg            *EdgeConfig
	// Options for highlighted edges visualisation.
	HighlightedEdgeCfg *EdgeConfig
	// Indicate whether shortcuts should be used.
	// Shortcut of a string is a prefix of a string that is unique
	// for a given set of strings.
	EnableShortcut     bool
}

var DefaultVisualizerConfig VisualizerConfig = VisualizerConfig{
	GlobalCfg:          &DefaultGlobalConfig,
	NodeCfg:            &DefaultNodeConfig,
	HighlightedNodeCfg: &DefaultHighlightedNodeConfig,
	EdgeCfg:            &DefaultEdgeConfig,
	HighlightedEdgeCfg: &DefaultHighlightedEdgeConfig,
	EnableShortcut:     false,
}

type GlobalConfig struct {
	// Title of an image.
	Name    string
	Dir     bool
	// Allow multigraph.
	Strict  bool
	// Background color.
	BgColor string
}

var DefaultGlobalConfig GlobalConfig = GlobalConfig{
	Name:    `"Routing Table"`,
	Dir:     true,
	Strict:  false, // Allow multigraphs
	BgColor: "black",
}

type NodeConfig struct {
	Shape     string
	Style     string
	FontSize  string
	FontColor string
	Color     string
	FillColor string
}

var DefaultNodeConfig NodeConfig = NodeConfig{
	Shape:     "circle",
	Style:     "filled",
	FontSize:  "12",
	FontColor: "black",
	Color:     "white",
	FillColor: "white",
}

var DefaultHighlightedNodeConfig NodeConfig = NodeConfig{
	Shape:     "circle",
	Style:     "filled",
	FontSize:  "12",
	FontColor: "black",
	Color:     "blue",
	FillColor: "blue",
}

type EdgeConfig struct {
	FontSize  string
	FontColor string
	Scale     string
	Dir       string
	Style     string
	Color     string
}

var DefaultEdgeConfig EdgeConfig = EdgeConfig{
	FontSize:  "12",
	FontColor: "gold",
	Scale:     "2.5",
	Dir:       "none",
	Style:     "solid",
	Color:     "white",
}

var DefaultHighlightedEdgeConfig EdgeConfig = EdgeConfig{
	FontSize:  "12",
	FontColor: "gold",
	Scale:     "2.5",
	Dir:       "none",
	Style:     "solid",
	Color:     "blue",
}

func SupportedFormatsAsMap() map[string]struct{} {
	rez := make(map[string]struct{})
	for _, format := range supportedFormats {
		rez[format] = struct{}{}
	}
	return rez
}

// SupportedFormats contains list of image formats that can be
// used for output.
func SupportedFormats() []string {
	return supportedFormats
}

var supportedFormats = []string{
	"bmp", "canon", "cgimage", "cmap", "cmapx", "cmapx_np", "dot", "eps", "exr",
	"fig", "gif", "gv", "icns", "ico", "imap", "imap_np", "ismap", "jp2", "jpe",
	"jpeg", "jpg", "pct", "pdf", "pic", "pict", "plain", "plain-ext", "png",
	"pov", "ps", "ps2", "psd", "sgi", "svg", "svgz", "tga", "tif", "tiff", "tk",
	"vml", "vmlz", "xdot", "xdot1.2", "xdot1.4",
}
