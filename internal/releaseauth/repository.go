package releaseauth

import (
	"encoding/json"
	"errors"

	"github.com/sigstore/sigstore-go/pkg/bundle"
)

// CandidateRepository reads an UNVERIFIED certificate solely to choose one of
// the caller's explicit repository identities. The caller must then Verify the
// entire bundle exactly once with that repository, workflow, tag and source.
// Neither the certificate nor a failed verification can introduce another
// trusted repository or trigger a signature-policy fallback.
func CandidateRepository(tag string, encoded []byte, repositories ...string) (string, error) {
	if len(encoded) == 0 || len(encoded) > 4<<20 {
		return "", errors.New("missing or oversized release provenance")
	}
	var signed bundle.Bundle
	if err := json.Unmarshal(encoded, &signed); err != nil {
		return "", errors.New("invalid release provenance bundle")
	}
	content, err := signed.VerificationContent()
	if err != nil {
		return "", errors.New("invalid release provenance certificate")
	}
	certificate := content.Certificate()
	if certificate == nil || len(certificate.URIs) != 1 {
		return "", errors.New("release provenance requires one exact workflow certificate identity")
	}
	for _, repo := range repositories {
		identity, err := releaseIdentity(repo, tag)
		if err != nil {
			return "", err
		}
		if certificate.URIs[0].String() == identity.SubjectAlternativeName.SubjectAlternativeName {
			return repo, nil
		}
	}
	return "", errors.New("release provenance certificate does not match an authorized repository workflow and tag")
}
