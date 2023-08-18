package dht_pb

import (
	"encoding/json"
	"fmt"
)

type byteString string

func (b *byteString) MarshalTo(data []byte) (int, error) {
	return copy(data, *b), nil
}

func (b *byteString) Size() int {
	return len(*b)
}

func (b *byteString) Marshal() ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("empty byte string")
	}
	return []byte(*b), nil
}

func (b *byteString) Unmarshal(data []byte) error {
	*b = byteString(data)
	return nil
}

func (b *byteString) Equal(other *byteString) bool {
	if b != nil && other != nil {
		return *b == *other
	}
	return b == nil && other == nil
}

func (b *byteString) MarshalJSON() ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("empty byte string")
	}
	return json.Marshal([]byte(*b))
}

func (b *byteString) UnmarshalJSON(data []byte) error {
	var buf []byte
	err := json.Unmarshal(data, &buf)
	if err != nil {
		return err
	}
	*b = byteString(buf)
	return nil
}
