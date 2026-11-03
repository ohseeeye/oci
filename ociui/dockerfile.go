// Modified for dreg: local imports; see UPSTREAM.md.

package ociui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	rendering "github.com/ohseeeye/dreg/internal/explore/internal/render"
	"github.com/ohseeeye/oci"
)

// imageConfig holds only the history fields needed by the Dockerfile renderer.
// The original config JSON is used for all other rendering.
type imageConfig struct {
	History []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	} `json:"history"`
}

var whitespaceRegex = regexp.MustCompile(`( )(?:    )+`)

func whitespaceRepl(in []byte) []byte {
	return bytes.Replace(in, []byte(" "), []byte(" \\\n"), 1)
}

func (h *handler) renderDockerfileSchema1(w http.ResponseWriter, r *http.Request, b []byte, repo string) error {
	var m oci.Schema1Manifest
	err := json.Unmarshal(b, &m)
	if err != nil {
		return err
	}

	args := []string{}
	fmt.Fprintf(w, "<table>\n")
	for i := len(m.History) - 1; i >= 0; i-- {
		compat := m.History[i]
		// The compatibility document is JSON inside a string. Only command
		// history is needed here; the manifest itself is modeled by oci.
		var c struct {
			ContainerConfig struct {
				Cmd []string `json:"Cmd"`
			} `json:"container_config"`
		}
		if err := json.Unmarshal([]byte(compat.V1Compatibility), &c); err != nil {
			return err
		}

		cb := strings.Join(c.ContainerConfig.Cmd, " ")

		href, digest, size := "", "", int64(0)
		if i < len(m.FSLayers) {
			layerDigest := m.FSLayers[i].BlobSum
			href = fmt.Sprintf("/fs/%s@%s/", repo, layerDigest)
			digest = layerDigest.String()
			if _, after, ok := strings.Cut(digest, ":"); ok {
				if len(after) > 8 {
					digest = after[:8]
				}
			}

			if layerDigest != emptyDigest && layerDigest.Validate() == nil {
				if desc, err := h.registry.ResolveBlob(r.Context(), repo, layerDigest); err == nil {
					size = desc.Size
				}
			}
		}

		fmt.Fprintf(w, "<tr>\n")
		fmt.Fprintf(w, "<td class=\"noselect\"><p><a href=%q><em>%s</em></a></p></td>\n", href, digest)
		if size != 0 {
			human := rendering.ByteSize(uint64(size))
			fmt.Fprintf(w, "<td class=\"noselect\"><p title=\"%d bytes\">%s</p></td>\n", size, human)
		} else {
			fmt.Fprintf(w, "<td></td>\n")
		}

		fmt.Fprintf(w, "<td>\n<pre>\n")
		args, err = renderArg(w, cb, args)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "</pre>\n</td>\n")
		fmt.Fprintf(w, "</tr>\n")
	}
	fmt.Fprintf(w, "</table>\n")
	return nil
}

// TODO: add timestamps
func renderDockerfile(w io.Writer, b []byte, m *oci.IndexOrManifest, repo string) error {
	var cf imageConfig
	if err := json.Unmarshal(b, &cf); err != nil {
		return err
	}
	var err error

	fmt.Fprintf(w, "<table>\n")
	args := []string{}
	index := -1
	for _, hist := range cf.History {
		digest := ""
		href := ""
		size := int64(0)
		if m != nil {
			if !hist.EmptyLayer {
				index++
				if index < len(m.Layers) {
					desc := m.Layers[index]
					digest = desc.Digest.String()
					href = fmt.Sprintf("/fs/%s/?mt=%s", repo+"@"+digest, desc.MediaType)
					if _, after, ok := strings.Cut(digest, ":"); ok {
						if len(after) > 8 {
							digest = after[:8]
						}
					}
					size = m.Layers[index].Size
				}
			}
		}
		fmt.Fprintf(w, "<tr>\n")
		fmt.Fprintf(w, "<td class=\"noselect\"><p><a href=%q><em>%s</em></a></p></td>\n", href, digest)
		if size != 0 {
			human := rendering.ByteSize(uint64(size))
			fmt.Fprintf(w, "<td class=\"noselect\"><p title=\"%d bytes\">%s</p></td>\n", size, human)
		} else {
			fmt.Fprintf(w, "<td></td>\n")
		}

		cb := hist.CreatedBy
		fmt.Fprintf(w, "<td>\n<pre>\n")

		args, err = renderArg(w, cb, args)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "</pre>\n</td>\n")
		fmt.Fprintf(w, "</tr>\n")
	}
	fmt.Fprintf(w, "</table>\n")
	return nil
}

func renderArg(w io.Writer, cb string, args []string) ([]string, error) {
	var sb strings.Builder
	// Attempt to handle weird ARG stuff.
	maybe := strings.TrimSpace(strings.TrimPrefix(cb, "/bin/sh -c #(nop)"))
	if before, after, ok := strings.Cut(maybe, "ARG "); ok && before == "" {
		args = append(args, after)
	} else if strings.HasPrefix(cb, "|") {
		if _, cb, ok = strings.Cut(cb, " "); ok {
			for _, arg := range args {
				cb = strings.TrimSpace(strings.TrimPrefix(cb, arg))
			}

			// Hack around array syntax.
			if !strings.HasPrefix(cb, "/bin/sh -c ") {
				cb = "/bin/sh -c " + cb
			}
		}
	}
	if err := renderCreatedBy(&sb, []byte(cb)); err != nil {
		return nil, err
	}
	if _, err := sb.Write([]byte("\n\n")); err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return nil, err
	}

	return args, nil
}

const (
	winPrefix = `powershell -Command $ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';`
	linPrefix = `/bin/sh -c`
)

func renderCreatedBy(w io.Writer, b []byte) error {
	// Heuristically try to format this correctly.
	for _, prefix := range []string{linPrefix, winPrefix} {
		b = bytes.TrimPrefix(b, []byte(prefix+" #(nop)"))
		if bytes.HasPrefix(b, []byte(prefix)) {
			b = bytes.Replace(b, []byte(prefix), []byte("RUN"), 1)
		}
	}
	b = bytes.ReplaceAll(b, []byte(" \t"), []byte(" \\\n\t"))
	b = bytes.ReplaceAll(b, []byte("&&\t"), []byte("\\\n&&\t"))
	b = whitespaceRegex.ReplaceAllFunc(b, whitespaceRepl)
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, []byte("EXPOSE")) {
		// Turn the map version into the dockerfile version
		b = bytes.TrimSuffix(b, []byte("]"))
		b = bytes.Replace(b, []byte("map["), []byte(""), 1)
		b = bytes.ReplaceAll(b, []byte(":{}"), []byte(""))
	}
	if bytes.HasPrefix(b, []byte("|")) {
		if _, after, ok := bytes.Cut(b, []byte("/bin/sh -c")); ok {
			b = []byte("RUN")
			b = append(b, after...)
		}
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("Write: %w", err)
	}
	return nil
}
