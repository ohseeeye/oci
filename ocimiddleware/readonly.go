// Copyright 2023 CUE Labs AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ocimiddleware

import (
	"context"
	"iter"

	"github.com/ohseeeye/oci"
)

// ReadOnly returns a registry implementation that returns
// an "operation unsupported" error from all entry points that
// mutate the registry.
func ReadOnly(r oci.Registry) oci.Registry {
	return &readOnlyRegistry{
		Reader: r,
		Lister: r,
		r:      r,
	}
}

type readOnlyRegistry struct {
	oci.Reader
	oci.Lister
	readOnlyUnsupported
	r oci.Registry
}

// Keep unsupported defaults below Reader and Lister in method promotion.
type readOnlyUnsupported struct {
	*oci.Funcs
}

var _ oci.OhSeeEye = (*readOnlyRegistry)(nil)

func (r *readOnlyRegistry) TagHistory(ctx context.Context, repo, tag string, params *oci.TagHistoryParameters) iter.Seq2[oci.Descriptor, error] {
	return forwardTagHistory(ctx, r.r, repo, tag, params)
}
