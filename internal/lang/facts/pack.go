package facts

import (
	"encoding/binary"
	"fmt"
)

// PackRefs encodes a file's references into one compact byte slice. The
// indexer parses every file before it can resolve any reference, so a cold run
// on a large tree would otherwise retain one ~160-byte Ref struct per
// reference (1.17M refs / ~258 MiB on the pinned Kibana tree). The packed form
// is roughly a third of that and is decoded one file at a time during
// resolution, which keeps the live set bounded without dropping a field or
// changing resolution.
//
// The encoding is deliberately private and versionless: producer and consumer
// are the same process and the same build. A field added to Ref must be added
// here and to the round-trip test in the same change.
func PackRefs(refs []Ref) []byte {
	if len(refs) == 0 {
		return nil
	}
	// A rough size avoids repeated growth: seven of the eight strings are
	// usually short or empty, and the fixed part is 8 length prefixes + 3
	// numbers.
	size := binary.MaxVarintLen64
	for _, ref := range refs {
		size += 1 + 8 + 3 + len(ref.Kind) + len(ref.Name) + len(ref.Form) +
			len(ref.Receiver) + len(ref.ReceiverType) + len(ref.ReceiverBound) +
			len(ref.ReceiverFrom) + len(ref.Enclosing)
	}
	buf := make([]byte, 0, size)
	buf = binary.AppendUvarint(buf, uint64(len(refs)))
	for _, ref := range refs {
		buf = appendRef(buf, ref)
	}
	return buf
}

func appendRef(buf []byte, ref Ref) []byte {
	if ref.ReceiverParam {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	for _, s := range [...]string{
		ref.Kind, ref.Name, ref.Form, ref.Receiver, ref.ReceiverType,
		ref.ReceiverBound, ref.ReceiverFrom, ref.Enclosing,
	} {
		buf = binary.AppendUvarint(buf, uint64(len(s)))
		buf = append(buf, s...)
	}
	buf = binary.AppendVarint(buf, int64(ref.Arity))
	buf = binary.AppendVarint(buf, int64(ref.Line))
	return buf
}

// UnpackRefs decodes one file's packed references. It is the inverse of
// PackRefs; malformed input is an error rather than a partial answer.
func UnpackRefs(data []byte) ([]Ref, error) {
	if len(data) == 0 {
		return nil, nil
	}
	count, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, fmt.Errorf("refs: bad count")
	}
	data = data[n:]
	// Each ref has at least 1 flag byte, 8 one-byte string lengths, and two
	// one-byte numbers. Rejecting an impossible count keeps corrupt input from
	// forcing a huge allocation.
	if count > uint64(len(data))/11+1 {
		return nil, fmt.Errorf("refs: count %d exceeds %d bytes", count, len(data))
	}

	refs := make([]Ref, 0, int(count))
	for i := uint64(0); i < count; i++ {
		var ref Ref
		if len(data) == 0 {
			return nil, fmt.Errorf("refs: truncated at %d of %d", i, count)
		}
		ref.ReceiverParam = data[0] == 1
		data = data[1:]

		fields := [...]*string{
			&ref.Kind, &ref.Name, &ref.Form, &ref.Receiver, &ref.ReceiverType,
			&ref.ReceiverBound, &ref.ReceiverFrom, &ref.Enclosing,
		}
		for _, field := range fields {
			s, rest, err := readRefString(data)
			if err != nil {
				return nil, fmt.Errorf("refs: %w", err)
			}
			*field = s
			data = rest
		}

		arity, n := binary.Varint(data)
		if n <= 0 {
			return nil, fmt.Errorf("refs: bad arity at %d", i)
		}
		data = data[n:]
		line, n := binary.Varint(data)
		if n <= 0 {
			return nil, fmt.Errorf("refs: bad line at %d", i)
		}
		data = data[n:]
		ref.Arity = int(arity)
		ref.Line = int(line)
		refs = append(refs, ref)
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("refs: %d trailing bytes", len(data))
	}
	return refs, nil
}

func readRefString(data []byte) (string, []byte, error) {
	length, n := binary.Uvarint(data)
	if n <= 0 {
		return "", nil, fmt.Errorf("bad string length")
	}
	data = data[n:]
	if length > uint64(len(data)) {
		return "", nil, fmt.Errorf("string length %d > %d remaining bytes", length, len(data))
	}
	return string(data[:length]), data[length:], nil
}
