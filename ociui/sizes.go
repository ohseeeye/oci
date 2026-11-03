package ociui

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/pkg/ociref"
)

// An image index has child manifests, not filesystem layers. Let the user
// choose a platform instead of trying to flatten the index as an empty image
// or downloading every platform's layers to produce a combined size listing.
func (h *handler) renderSizeIndex(w http.ResponseWriter, r *http.Request, ref ociref.Reference, desc *manifestData, children []oci.Descriptor) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}
	header := h.manifestHeader(ref, desc.Descriptor)
	header.JQ = crane("manifest") + " " + ref.String() + ` | jq '.manifests[] | {platform, digest, size}'`
	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}
	if len(children) == 0 {
		fmt.Fprint(w, "<p>This index contains no manifests.</p>\n", footer)
		return nil
	}

	fmt.Fprint(w, "<h3>Platforms</h3>\n<p>Choose a platform to list its filesystem files by size. Byte counts below are manifest sizes, not unpacked filesystem sizes.</p>\n")
	fmt.Fprint(w, "<table>\n<thead><tr><th>Platform</th><th>Manifest bytes</th><th>Manifest</th></tr></thead>\n<tbody>\n")
	for _, child := range children {
		name := "unspecified"
		if p := child.Platform; p != nil && (p.OS != "" || p.Architecture != "") {
			name = p.OS + "/" + p.Architecture
			if p.Variant != "" {
				name += "/" + p.Variant
			}
			if p.OSVersion != "" {
				name += " (" + p.OSVersion + ")"
			}
		}
		childRef := ref.Repository + "@" + child.Digest.String()
		query := r.URL.Query()
		query.Set("mt", child.MediaType)
		query.Set("size", strconv.FormatInt(child.Size, 10))
		// Preserve search/path/all options so they apply once a platform is
		// selected. There is no host-platform inference or eager layer fetch.
		target := withQuery(localURL("sizes/"+childRef, nil), query)
		fmt.Fprintf(w, "<tr><td><a href=\"%s\">%s</a></td><td title=\"%s\">%d</td><td><a href=\"%s\">%s</a></td></tr>\n",
			html.EscapeString(target), html.EscapeString(strings.Trim(name, "/")), renderSize(child.Size), child.Size,
			html.EscapeString(imageURL(childRef)), html.EscapeString(child.Digest.String()))
	}
	fmt.Fprint(w, "</tbody>\n</table>\n", footer)
	return nil
}
