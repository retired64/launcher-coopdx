package devicecopy

import (
	"sort"
	"strings"
)

// LocalFile is one file the receiver already has.
type LocalFile struct {
	Rel    string
	Size   int64
	SHA256 string
}

// LocalMod is one mod/pack on the receiver.
type LocalMod struct {
	ID       string
	Type     string
	Location string
	Name     string
	Enabled  bool
	Files    []LocalFile
}

// LocalState is the receiver's inventory used to build the review.
type LocalState struct {
	Mods      []LocalMod
	SaveStars [4]*int
}

// Plan is the result of the receiver's choices.
type Plan struct {
	Choice      Choice
	Mods        []int
	Want        []int
	Bytes       int64
	EnableMods  []string
	EnablePacks []string
	Review      Review
}

func modKey(m Mod) string { return strings.ToLower(m.Type) + ":" + strings.ToLower(m.ID) }

func localKey(m LocalMod) string { return strings.ToLower(m.Type) + ":" + strings.ToLower(m.ID) }

func modSize(m Mod) int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// BuildPlan applies the reference client's selection rules.
//
// It deliberately fixes one dangerous edge case: when the player unticks
// "Mods", the enabled-mod block is left alone instead of enabling mods whose
// files were not copied. The rest of the selection and review semantics match
// the observed client.
func BuildPlan(m *Manifest, local LocalState, senderStars [4]*int, choice Choice, route string) *Plan {
	byKey := map[string][]LocalMod{}
	for _, lm := range local.Mods {
		key := localKey(lm)
		byKey[key] = append(byKey[key], lm)
	}
	userFor := func(sm Mod) *LocalMod {
		for _, lm := range byKey[modKey(sm)] {
			if lm.Location == "user" {
				copy := lm
				return &copy
			}
		}
		return nil
	}
	present := func(sm Mod) bool { return len(byKey[modKey(sm)]) > 0 }

	candidates := make([]int, 0, len(m.Mods))
	for i, sm := range m.Mods {
		if sm.Location != "user" {
			continue
		}
		rm := userFor(sm)
		if rm != nil && sameFiles(sm.Files, rm.Files) {
			continue
		}
		candidates = append(candidates, i)
	}

	selected := append([]int(nil), candidates...)
	skipped := []string{}
	if route == "relay" {
		order := append([]int(nil), candidates...)
		sort.SliceStable(order, func(i, j int) bool {
			return modSize(m.Mods[order[i]]) < modSize(m.Mods[order[j]])
		})
		budget := int64(RelayBudget)
		selected = nil
		for _, index := range order {
			size := modSize(m.Mods[index])
			if size <= budget {
				budget -= size
				selected = append(selected, index)
			} else {
				skipped = append(skipped, m.Mods[index].Name)
			}
		}
		sort.Ints(selected)
	}

	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	selectedOrPresent := func(index int) bool {
		return selectedSet[index] || present(m.Mods[index])
	}

	added := []string{}
	updated := []string{}
	for _, index := range selected {
		if userFor(m.Mods[index]) != nil {
			updated = append(updated, m.Mods[index].Name)
		} else {
			added = append(added, m.Mods[index].Name)
		}
	}

	enableMods := []string{}
	enablePacks := []string{}
	missing := []string{}
	if choice.Mods {
		for i, sm := range m.Mods {
			if !sm.Enabled {
				continue
			}
			if !selectedOrPresent(i) {
				if !contains(skipped, sm.Name) {
					missing = append(missing, sm.Name)
				}
				continue
			}
			if sm.Type == "pack" {
				enablePacks = append(enablePacks, sm.ID)
			} else {
				enableMods = append(enableMods, sm.ID)
			}
		}
	} else {
		for _, sm := range m.Mods {
			if sm.Enabled && !present(sm) {
				missing = append(missing, sm.Name)
			}
		}
	}

	senderKeys := map[string]bool{}
	for _, sm := range m.Mods {
		senderKeys[modKey(sm)] = true
	}
	turnedOff := []string{}
	for _, lm := range local.Mods {
		if lm.Enabled && !senderKeys[localKey(lm)] {
			turnedOff = append(turnedOff, lm.Name)
		}
	}

	compiled := []string{}
	if choice.Mods {
		for _, index := range selected {
			sm := m.Mods[index]
			for _, f := range sm.Files {
				if f.Code || strings.HasSuffix(strings.ToLower(f.Rel), ".luac") {
					compiled = append(compiled, sm.Name)
					break
				}
			}
		}
	}

	want := []int{}
	var bytes int64
	if choice.Mods {
		flat := m.Flatten()
		for i, f := range flat {
			if selectedSet[f.Mod] {
				want = append(want, i)
				bytes += f.Ref.Size
			}
		}
	}

	look := LookReview{Colors: map[string]string{}, Presets: []string{}}
	if m.Player != nil {
		look.Name = m.Player.Name
		look.Model = m.Player.Model
		look.Colors = m.Player.Colors
	}
	for _, p := range m.Presets {
		look.Presets = append(look.Presets, p.Name)
	}

	slots := make([]SaveSlotReview, 4)
	for i := 0; i < 4; i++ {
		slots[i] = SaveSlotReview{Slot: i, Stars: senderStars[i], Here: local.SaveStars[i]}
	}

	review := Review{
		From: m.Device,
		Mods: ModsReview{
			Added:     added,
			Updated:   updated,
			On:        len(enableMods) + len(enablePacks),
			Bytes:     bytes,
			TurnedOff: turnedOff,
			Missing:   missing,
			Skipped:   skipped,
			Compiled:  compiled,
		},
		Look: look,
		Saves: SavesReview{
			Slots:    slots,
			ModSaves: len(m.Savs),
		},
	}
	return &Plan{
		Choice:      choice,
		Mods:        selected,
		Want:        want,
		Bytes:       bytes,
		EnableMods:  enableMods,
		EnablePacks: enablePacks,
		Review:      review,
	}
}

func sameFiles(sender []File, local []LocalFile) bool {
	if len(sender) != len(local) {
		return false
	}
	byRel := make(map[string]LocalFile, len(local))
	for _, f := range local {
		byRel[strings.ToLower(f.Rel)] = f
	}
	for _, f := range sender {
		lf, ok := byRel[strings.ToLower(f.Rel)]
		if !ok || lf.Size != f.Size || lf.SHA256 != f.SHA256 {
			return false
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
