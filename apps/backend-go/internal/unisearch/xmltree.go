package unisearch

import (
	"bytes"
	"encoding/xml"
	"io"
)

// node is the slice of xml.etree.ElementTree the sources use: Text is Element.text (the character data before the
// first child element, or all of it for a leaf) and HasText tells None from "".
type node struct {
	Space, Local string
	Attr         map[string]string
	Text         string
	HasText      bool
	Kids         []*node
}

// parseXML parses a document like ET.fromstring (a malformed document is an error).
func parseXML(b []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	var root *node
	var stack []*node
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{Space: t.Name.Space, Local: t.Name.Local, Attr: map[string]string{}}
			for _, a := range t.Attr {
				n.Attr[a.Name.Local] = a.Value
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.Kids = append(p.Kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				c := stack[len(stack)-1]
				if len(c.Kids) == 0 {
					c.Text += string(t)
					c.HasText = c.Text != ""
				}
			}
		}
	}
	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return root, nil
}

// child is find('ns:name') / find('name'): the first direct child (space "" matches the no-namespace form only).
func (n *node) child(space, local string) *node {
	for _, k := range n.Kids {
		if k.Local == local && k.Space == space {
			return k
		}
	}
	return nil
}

func (n *node) children(space, local string) []*node {
	var out []*node
	for _, k := range n.Kids {
		if k.Local == local && k.Space == space {
			out = append(out, k)
		}
	}
	return out
}

// descendants is findall('.//name'): every element below n in document order.
func (n *node) descendants(space, local string) []*node {
	var out []*node
	var walk func(*node)
	walk = func(p *node) {
		for _, k := range p.Kids {
			if k.Local == local && k.Space == space {
				out = append(out, k)
			}
			walk(k)
		}
	}
	walk(n)
	return out
}
