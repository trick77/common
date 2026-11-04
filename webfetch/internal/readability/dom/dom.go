// Package dom is the subset of github.com/go-shiori/dom
// (v0.0.0-20230515143342-73569d674e1c, MIT, see LICENSE) that readability
// uses: DOM-style helpers over golang.org/x/net/html. Function bodies are
// unchanged, except that ReplaceChild returns on a nil parent instead of
// panicking; unused helpers and the chardet-based Parse are dropped.
package dom

import (
	"bytes"
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// QuerySelectorAll returns array of document's elements that match
// the specified group of selectors.
func QuerySelectorAll(doc *html.Node, selectors string) []*html.Node {
	matcher, err := cascadia.ParseGroup(selectors)
	if err != nil {
		return nil
	}

	return cascadia.QueryAll(doc, matcher)
}

// GetElementsByTagName returns a collection of all elements in the document with
// the specified tag name, as an array of Node object.
// The special tag "*" will represents all elements.
func GetElementsByTagName(doc *html.Node, tagName string) []*html.Node {
	var results []*html.Node
	var finder func(*html.Node)

	finder = func(node *html.Node) {
		if node.Type == html.ElementNode && (tagName == "*" || node.Data == tagName) {
			results = append(results, node)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			finder(child)
		}
	}

	for child := doc.FirstChild; child != nil; child = child.NextSibling {
		finder(child)
	}

	return results
}

// CreateElement creates a new ElementNode with specified tag.
func CreateElement(tagName string) *html.Node {
	return &html.Node{
		Type: html.ElementNode,
		Data: tagName,
	}
}

// CreateTextNode creates a new Text node.
func CreateTextNode(data string) *html.Node {
	return &html.Node{
		Type: html.TextNode,
		Data: data,
	}
}

// TagName returns the tag name of a Node.
// If it's not ElementNode, return empty string.
func TagName(node *html.Node) string {
	if node == nil {
		return ""
	}

	if node.Type != html.ElementNode {
		return ""
	}

	return node.Data
}

// GetAttribute returns the value of a specified attribute on
// the element. If the given attribute does not exist, the value
// returned will be an empty string.
func GetAttribute(node *html.Node, attrName string) string {
	for i := 0; i < len(node.Attr); i++ {
		if node.Attr[i].Key == attrName {
			return node.Attr[i].Val
		}
	}
	return ""
}

// SetAttribute sets attribute for node. If attribute already exists,
// it will be replaced.
func SetAttribute(node *html.Node, attrName string, attrValue string) {
	attrIdx := -1
	for i := 0; i < len(node.Attr); i++ {
		if node.Attr[i].Key == attrName {
			attrIdx = i
			break
		}
	}

	if attrIdx >= 0 {
		node.Attr[attrIdx].Val = attrValue
	} else {
		node.Attr = append(node.Attr, html.Attribute{
			Key: attrName,
			Val: attrValue,
		})
	}
}

// RemoveAttribute removes attribute with given name.
func RemoveAttribute(node *html.Node, attrName string) {
	attrIdx := -1
	for i := 0; i < len(node.Attr); i++ {
		if node.Attr[i].Key == attrName {
			attrIdx = i
			break
		}
	}

	if attrIdx >= 0 {
		a := node.Attr
		a = append(a[:attrIdx], a[attrIdx+1:]...)
		node.Attr = a
	}
}

// HasAttribute returns a Boolean value indicating whether the
// specified node has the specified attribute or not.
func HasAttribute(node *html.Node, attrName string) bool {
	for i := 0; i < len(node.Attr); i++ {
		if node.Attr[i].Key == attrName {
			return true
		}
	}
	return false
}

// TextContent returns the text content of the specified node,
// and all its descendants.
func TextContent(node *html.Node) string {
	var buffer bytes.Buffer
	var finder func(*html.Node)

	finder = func(n *html.Node) {
		if n.Type == html.TextNode {
			buffer.WriteString(n.Data)
		}

		for child := n.FirstChild; child != nil; child = child.NextSibling {
			finder(child)
		}
	}

	finder(node)
	return buffer.String()
}

// InnerHTML returns the HTML content (inner HTML) of an element.
// The returned HTML value is escaped.
func InnerHTML(node *html.Node) string {
	var err error
	var buffer bytes.Buffer

	if node == nil {
		return ""
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		err = html.Render(&buffer, child)
		if err != nil {
			return ""
		}
	}

	return strings.TrimSpace(buffer.String())
}

// DocumentElement returns the Element that is the root element
// of the document. Since we are working with HTML document,
// the root will be <html> element for HTML documents).
func DocumentElement(doc *html.Node) *html.Node {
	if nodes := GetElementsByTagName(doc, "html"); len(nodes) > 0 {
		return nodes[0]
	}
	return nil
}

// ID returns the value of the id attribute of the specified element.
func ID(node *html.Node) string {
	id := GetAttribute(node, "id")
	id = strings.TrimSpace(id)
	return id
}

// ClassName returns the value of the class attribute of
// the specified element.
func ClassName(node *html.Node) string {
	className := GetAttribute(node, "class")
	className = strings.TrimSpace(className)
	className = strings.Join(strings.Fields(className), " ")
	return className
}

// Children returns an HTMLCollection of the direct child elements of Node.
func Children(node *html.Node) []*html.Node {
	var children []*html.Node
	if node == nil {
		return nil
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			children = append(children, child)
		}
	}

	return children
}

// ChildNodes returns list of a node's direct children.
func ChildNodes(node *html.Node) []*html.Node {
	var childNodes []*html.Node
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		childNodes = append(childNodes, child)
	}
	return childNodes
}

// FirstElementChild returns the object's first child Element,
// or nil if there are no child elements.
func FirstElementChild(node *html.Node) *html.Node {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			return child
		}
	}
	return nil
}

// PreviousElementSibling returns the the Element immediately prior
// to the specified one in its parent's children list, or null if
// the specified element is the first one in the list.
func PreviousElementSibling(node *html.Node) *html.Node {
	for sibling := node.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
		if sibling.Type == html.ElementNode {
			return sibling
		}
	}
	return nil
}

// NextElementSibling returns the Element immediately following
// the specified one in its parent's children list, or nil if the
// specified Element is the last one in the list.
func NextElementSibling(node *html.Node) *html.Node {
	for sibling := node.NextSibling; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type == html.ElementNode {
			return sibling
		}
	}
	return nil
}

// AppendChild adds a node to the end of the list of children of a
// specified parent node. If the given child is a reference to an
// existing node in the document, AppendChild() moves it from its
// current position to the new position.
func AppendChild(node *html.Node, child *html.Node) {
	// Make sure node is not void
	if !IsVoidElement(node) {
		DetachChild(child)
		node.AppendChild(child)
	}
}

// ReplaceChild replaces a child node within the given (parent) node.
// If the new child is already exist in document, ReplaceChild() will move it
// from its current position to replace old child. Returns both the new and old child.
//
// TODO: I'm note sure but I *think* there are some issues here. Check later I guess.
func ReplaceChild(parent *html.Node, newChild *html.Node, oldChild *html.Node) (*html.Node, *html.Node) {
	// Make sure parent is specified. Upstream's "parent == nil &&
	// !IsVoidElement(parent)" panicked on nil and never checked void.
	if parent == nil {
		return newChild, oldChild
	}

	// Make sure the specified parent IS the parent of the old child
	if oldChild.Parent != parent {
		return newChild, oldChild
	}

	// Detach the new child
	DetachChild(newChild)
	parent.InsertBefore(newChild, oldChild)
	parent.RemoveChild(oldChild)
	return newChild, oldChild
}

// IncludeNode determines if node is included inside nodeList.
func IncludeNode(nodeList []*html.Node, node *html.Node) bool {
	for i := 0; i < len(nodeList); i++ {
		if nodeList[i] == node {
			return true
		}
	}
	return false
}

// Clone returns a clone of the node and (if specified) its children.
// However, it will be detached from the original's parents and siblings.
func Clone(src *html.Node, deep bool) *html.Node {
	clone := &html.Node{
		Type:     src.Type,
		DataAtom: src.DataAtom,
		Data:     src.Data,
		Attr:     append([]html.Attribute{}, src.Attr...),
	}

	if deep {
		for child := src.FirstChild; child != nil; child = child.NextSibling {
			clone.AppendChild(Clone(child, deep))
		}
	}

	return clone
}

// IsVoidElement check whether a node can have any contents or not.
// Return true if element is void (can't have any children).
func IsVoidElement(n *html.Node) bool {
	// If it's not element, it's void
	if n.Type != html.ElementNode {
		return true
	}

	// Check tag name
	switch n.Data {
	case "area", "base", "br", "col", "embed", "hr",
		"img", "input", "keygen", "link", "meta",
		"param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

// DetachChild unlinks child from its parent and siblings.
func DetachChild(child *html.Node) {
	if child.Parent != nil || child.PrevSibling != nil || child.NextSibling != nil {
		if child.Parent != nil {
			if child.Parent.FirstChild == child {
				child.Parent.FirstChild = child.NextSibling
			}

			if child.Parent.LastChild == child {
				child.Parent.LastChild = child.PrevSibling
			}
		}

		if child.PrevSibling != nil {
			child.PrevSibling.NextSibling = child.NextSibling
		}

		if child.NextSibling != nil {
			child.NextSibling.PrevSibling = child.PrevSibling
		}

		child.Parent = nil
		child.PrevSibling = nil
		child.NextSibling = nil
	}
}
