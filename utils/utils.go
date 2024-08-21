package utils

import (
	"encoding/hex"
	"strings"

	log "github.com/sirupsen/logrus"
)

// sliceContains reports whether the provided string is present in the given slice of strings.
func SliceContains(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// MustParseHex will parse a string into hex
func MustParseHex(hexString string) []byte {
	data, err := hex.DecodeString(strings.Replace(hexString, "0x", "", -1))
	if err != nil {
		log.Fatal(err)
	}
	return data
}

func BitAtVector(b []byte, i int) bool {
	bb := b[i/8]
	return (bb & (1 << uint(i%8))) > 0
}

func BitAtVectorReversed(b []byte, i int) bool {
	bb := b[i/8]
	return (bb & (1 << uint(7-(i%8)))) > 0
}

func SyncCommitteeParticipation(bits []byte, syncCommitteeSize uint64) float64 {
	participating := 0
	for i := 0; i < int(syncCommitteeSize); i++ {
		if BitAtVector(bits, i) {
			participating++
		}
	}
	return float64(participating) / float64(syncCommitteeSize)
}
