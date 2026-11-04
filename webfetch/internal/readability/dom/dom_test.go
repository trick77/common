package dom

import (
	"testing"

	"golang.org/x/net/html"
)

func TestReplaceChild(t *testing.T) {
	parent := &html.Node{Type: html.ElementNode, Data: "div"}
	oldChild := &html.Node{Type: html.ElementNode, Data: "span"}
	newChild := &html.Node{Type: html.ElementNode, Data: "p"}
	parent.AppendChild(oldChild)

	ReplaceChild(parent, newChild, oldChild)
	if parent.FirstChild != newChild || parent.LastChild != newChild || oldChild.Parent != nil {
		t.Fatalf("ReplaceChild did not swap the child")
	}

	// A detached node has no parent: a no-op, not a nil dereference.
	detached := &html.Node{Type: html.ElementNode, Data: "a"}
	if n, o := ReplaceChild(detached.Parent, oldChild, detached); n != oldChild || o != detached {
		t.Fatalf("ReplaceChild(nil, …) = %v, %v", n, o)
	}
}
