package firstscreen

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
)

const (
	KindExact          = "exact"
	KindImageCandidate = "image_candidate"
	KindVideoCandidate = "video_candidate"
)

var M3Kinds = []string{KindExact, KindImageCandidate, KindVideoCandidate}

type CandidatePair struct {
	Kind           string
	ShaA           [64]byte
	ShaB           [64]byte
	Hamming        int
	DurationDiffMs int64
	QualityA       int
	QualityB       int
}

func newCandidatePair(kind string, s1, s2 [64]byte, hamming int, durDiffMs int64, q1, q2 int) CandidatePair {
	if bytes.Compare(s1[:], s2[:]) > 0 {
		s1, s2 = s2, s1
		q1, q2 = q2, q1
	}
	return CandidatePair{
		Kind:           kind,
		ShaA:           s1,
		ShaB:           s2,
		Hamming:        hamming,
		DurationDiffMs: durDiffMs,
		QualityA:       q1,
		QualityB:       q2,
	}
}

func (p CandidatePair) less(other CandidatePair) bool {
	if order := bytes.Compare(p.ShaA[:], other.ShaA[:]); order != 0 {
		return order < 0
	}
	return bytes.Compare(p.ShaB[:], other.ShaB[:]) < 0
}

func (p CandidatePair) scoreJSON(sideA bool) []byte {
	if p.Kind == KindExact {
		raw, _ := json.Marshal(struct {
			Basis string `json:"basis"`
		}{Basis: "sha512"})
		return raw
	}

	qualitySelf, qualityPeer := p.QualityA, p.QualityB
	peer := p.ShaB
	if !sideA {
		qualitySelf, qualityPeer = p.QualityB, p.QualityA
		peer = p.ShaA
	}
	peerText := hex.EncodeToString(peer[:])
	if p.Kind == KindVideoCandidate {
		raw, _ := json.Marshal(struct {
			Hamming      int    `json:"hamming"`
			DurationDiff int64  `json:"duration_diff_ms"`
			QualitySelf  int    `json:"quality_self"`
			QualityPeer  int    `json:"quality_peer"`
			PeerSHA512   string `json:"peer_sha512"`
		}{
			Hamming:      p.Hamming,
			DurationDiff: p.DurationDiffMs,
			QualitySelf:  qualitySelf,
			QualityPeer:  qualityPeer,
			PeerSHA512:   peerText,
		})
		return raw
	}
	raw, _ := json.Marshal(struct {
		Hamming     int    `json:"hamming"`
		QualitySelf int    `json:"quality_self"`
		QualityPeer int    `json:"quality_peer"`
		PeerSHA512  string `json:"peer_sha512"`
	}{
		Hamming:     p.Hamming,
		QualitySelf: qualitySelf,
		QualityPeer: qualityPeer,
		PeerSHA512:  peerText,
	})
	return raw
}
