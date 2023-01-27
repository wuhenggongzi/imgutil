package layout

import (
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type Identifier struct {
	Digest string
	Path   string
}

func newLayoutIdentifier(path string, hash v1.Hash) (Identifier, error) {
	return Identifier{
		Digest: hash.String(),
		Path:   path,
	}, nil
}

func (i Identifier) String() string {
	return fmt.Sprintf("%s@%s", i.Path, i.Digest)
}
