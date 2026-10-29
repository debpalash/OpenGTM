package unisearch

import (
	"strings"
	"testing"
)

// placeholder
func TestParseXMLCapsDepthNodesAndSize(t *testing.T) {
	deep := strings.Repeat("<a>", maxXMLDepth+1) + strings.Repeat("</a>", maxXMLDepth+1)
	if _, err := parseXML([]byte(deep)); err == nil {
		t.Error("nesting past the depth cap parsed")
	}
	ok := strings.Repeat("<a>", maxXMLDepth) + strings.Repeat("</a>", maxXMLDepth)
	if _, err := parseXML([]byte(ok)); err != nil {
		t.Errorf("nesting at the cap: %v", err)
	}
	wide := "<r>" + strings.Repeat("<e/>", maxXMLNodes+1) + "</r>"
	if _, err := parseXML([]byte(wide)); err == nil {
		t.Error("node count past the cap parsed")
	}
	big := "<r>" + strings.Repeat("x", maxXMLBytes) + "</r>"
	if _, err := parseXML([]byte(big)); err == nil {
		t.Error("body past the size cap parsed")
	}
	if _, err := parseXML([]byte("<r><e>t</e></r>")); err != nil {
		t.Errorf("normal document: %v", err)
	}
}
