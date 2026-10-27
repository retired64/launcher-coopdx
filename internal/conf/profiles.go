package conf

import (
	"encoding/json"
	"os"

	"github.com/retired64/coop-loader/internal/fsutil"
)

// Profiles stores named sets of enabled mods. Switching a profile rewrites
// the enable-mod block in sm64config.txt.
type Profiles struct {
	Active string              `json:"active"`
	Sets   map[string][]string `json:"sets"`
}

// LoadProfiles reads profiles.json. A missing file yields the default set.
func LoadProfiles(path string) (Profiles, error) {
	p := Profiles{Active: "Default", Sets: map[string][]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Profiles{Active: "Default", Sets: map[string][]string{}}, nil
	}
	if p.Sets == nil {
		p.Sets = map[string][]string{}
	}
	if _, ok := p.Sets[p.Active]; !ok {
		p.Active = "Default"
	}
	if _, ok := p.Sets[p.Active]; !ok {
		p.Sets["Default"] = nil
	}
	return p, nil
}

// Save writes profiles.json atomically.
func (p Profiles) Save(path string) error {
	if p.Sets == nil {
		p.Sets = map[string][]string{}
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

// Names returns the profile names in stable order.
func (p Profiles) Names() []string {
	out := make([]string, 0, len(p.Sets))
	for name := range p.Sets {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
