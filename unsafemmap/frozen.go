package unsafemmap

import (
	"fmt"

	"github.com/Voskan/cnfgo/cnfrt"
)

// FrozenSections extracts the FrozenMap stored under tag in a parsed image.
// The result aliases data.
func FrozenSections(data []byte, h cnfrt.Header, infos []cnfrt.SectionInfo, tag uint32) (cnfrt.FrozenSections, error) {
	ctrl, ok1 := cnfrt.FindSection(infos, cnfrt.SectionFrozenCtrl, tag)
	slots, ok2 := cnfrt.FindSection(infos, cnfrt.SectionFrozenSlots, tag)
	kv, ok3 := cnfrt.FindSection(infos, cnfrt.SectionFrozenData, tag)
	if !ok1 || !ok2 || !ok3 {
		return cnfrt.FrozenSections{}, fmt.Errorf("%w: frozen map %d is incomplete", cnfrt.ErrCorruptHeader, tag)
	}
	s, err := Cast[cnfrt.FrozenSlot](data, slots)
	if err != nil {
		return cnfrt.FrozenSections{}, err
	}
	return cnfrt.FrozenSections{
		Seed:      h.HashSeed,
		NumGroups: ctrl.Count,
		Len:       kv.Count,
		Ctrl:      cnfrt.SectionData(data, ctrl),
		Slots:     s,
		Data:      String(data, kv),
	}, nil
}

// FrozenMapFromImage loads a standalone FrozenMap image written by
// cnfrt.FrozenMap.WriteTo. With cnfrt.VerifyTrustedHeader and
// cnfrt.VerifyChecksum the map aliases data, which must not be modified
// while the map is in use; only the geometry is checked, under which lookups
// still terminate in bounded time. With cnfrt.VerifyUntrusted the map is
// built over a private aligned copy of data and every slot, key and value is
// validated (see cnfrt.FrozenMap.Validate), so lookups and iteration on
// arbitrary input cannot panic and later changes to data have no effect.
func FrozenMapFromImage[K comparable, V any](data []byte, mode cnfrt.VerifyMode, o *cnfrt.MapOptions[V]) (*cnfrt.FrozenMap[K, V], error) {
	if mode >= cnfrt.VerifyUntrusted {
		data = cnfrt.AlignedCopy(data)
	}
	return frozenMapFromImage[K, V](data, mode, o)
}

func frozenMapFromImage[K comparable, V any](data []byte, mode cnfrt.VerifyMode, o *cnfrt.MapOptions[V]) (*cnfrt.FrozenMap[K, V], error) {
	h, infos, err := cnfrt.ParseHeader(data, mode, cnfrt.FrozenMapSchema[K, V]())
	if err != nil {
		return nil, err
	}
	sec, err := FrozenSections(data, h, infos, 0)
	if err != nil {
		return nil, err
	}
	return cnfrt.FrozenMapFromSections[K, V](sec, o, mode >= cnfrt.VerifyUntrusted)
}

// OpenFrozenMap memory-maps a standalone FrozenMap image. The map decodes
// with cnfrt.MapOptions.CloneStrings set regardless of o, so keys and values
// returned by Get and All (including generated Compact values) are copies
// that remain valid after the mapping is released; the strings returned by
// GetEncoded and RawEntries still alias the mapping. The mapping is released
// by Close on the returned Mapping or, failing that, once the map becomes
// unreachable. The map must not be used after Close.
//
// With cnfrt.VerifyUntrusted the file is copied to the heap, the mapping is
// released before returning and the returned Mapping is nil.
func OpenFrozenMap[K comparable, V any](path string, mode cnfrt.VerifyMode, o *cnfrt.MapOptions[V]) (*cnfrt.FrozenMap[K, V], *Mapping, error) {
	m, err := Open(path)
	if err != nil {
		return nil, nil, err
	}
	if mode >= cnfrt.VerifyUntrusted {
		var data []byte
		err := FaultSafe(func() error {
			data = cnfrt.AlignedCopy(m.Bytes())
			return nil
		})
		m.Close()
		if err != nil {
			return nil, nil, err
		}
		fm, err := frozenMapFromImage[K, V](data, mode, o)
		return fm, nil, err
	}
	var opts cnfrt.MapOptions[V]
	if o != nil {
		opts = *o
	}
	opts.CloneStrings = true
	var fm *cnfrt.FrozenMap[K, V]
	err = FaultSafe(func() (err error) {
		fm, err = frozenMapFromImage[K, V](m.Bytes(), mode, &opts)
		return err
	})
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	AttachCleanup(fm, m)
	return fm, m, nil
}
