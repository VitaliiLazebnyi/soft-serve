package config

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

// keyExchangeCurve25519LibSSH is the pre-RFC name of curve25519-sha256.
// x/crypto implements it but lists it in neither SupportedAlgorithms nor
// InsecureAlgorithms; it offers it on its own whenever curve25519-sha256 is
// enabled. It is accepted here because OpenSSH configurations name it.
const keyExchangeCurve25519LibSSH = "curve25519-sha256@libssh.org"

// DefaultSSHKexAlgorithms returns the key exchange algorithms the SSH server
// offers when ssh.kex_algorithms is empty: every algorithm x/crypto
// considers secure, in its order of preference. This leaves out the SHA-1
// key exchanges (diffie-hellman-group14-sha1 and older) that x/crypto still
// offers by default.
func DefaultSSHKexAlgorithms() []string {
	return slices.Clone(ssh.SupportedAlgorithms().KeyExchanges)
}

// DefaultSSHCiphers returns the ciphers the SSH server offers when
// ssh.ciphers is empty: every cipher x/crypto considers secure, in its order
// of preference.
func DefaultSSHCiphers() []string {
	return slices.Clone(ssh.SupportedAlgorithms().Ciphers)
}

// DefaultSSHMACs returns the MACs the SSH server offers when ssh.macs is
// empty: every MAC x/crypto considers secure except the SHA-1 based
// hmac-sha1, which x/crypto still lists as supported. hmac-sha1-96 is in
// x/crypto's insecure list and is left out as well.
func DefaultSSHMACs() []string {
	return slices.DeleteFunc(slices.Clone(ssh.SupportedAlgorithms().MACs), func(m string) bool {
		return m == ssh.HMACSHA1
	})
}

// SSHKexAlgorithms returns the key exchange algorithms the SSH server
// offers: ssh.kex_algorithms, or DefaultSSHKexAlgorithms when it is empty.
func (c *SSHConfig) SSHKexAlgorithms() []string {
	if len(c.KexAlgorithms) == 0 {
		return DefaultSSHKexAlgorithms()
	}
	return slices.Clone(c.KexAlgorithms)
}

// SSHCiphers returns the ciphers the SSH server offers: ssh.ciphers, or
// DefaultSSHCiphers when it is empty.
func (c *SSHConfig) SSHCiphers() []string {
	if len(c.Ciphers) == 0 {
		return DefaultSSHCiphers()
	}
	return slices.Clone(c.Ciphers)
}

// SSHMACs returns the MACs the SSH server offers: ssh.macs, or
// DefaultSSHMACs when it is empty.
func (c *SSHConfig) SSHMACs() []string {
	if len(c.MACs) == 0 {
		return DefaultSSHMACs()
	}
	return slices.Clone(c.MACs)
}

// validateAlgorithms trims the algorithm lists, drops blank entries so an
// unset environment variable does not read as one empty name, and rejects
// names x/crypto does not implement. x/crypto silently ignores unknown
// names, so without this a typo would quietly shrink the list, or leave it
// empty and fail every handshake.
func (c *SSHConfig) validateAlgorithms() error {
	supported := ssh.SupportedAlgorithms()
	insecure := ssh.InsecureAlgorithms()
	for _, l := range []struct {
		name  string
		list  *[]string
		known []string
	}{
		{"ssh.kex_algorithms", &c.KexAlgorithms, slices.Concat(supported.KeyExchanges, insecure.KeyExchanges, []string{keyExchangeCurve25519LibSSH})},
		{"ssh.ciphers", &c.Ciphers, slices.Concat(supported.Ciphers, insecure.Ciphers)},
		{"ssh.macs", &c.MACs, slices.Concat(supported.MACs, insecure.MACs)},
	} {
		names := make([]string, 0, len(*l.list))
		for _, n := range *l.list {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if !slices.Contains(l.known, n) {
				return fmt.Errorf("%s: unknown algorithm %q; valid values are %s",
					l.name, n, strings.Join(l.known, ", "))
			}
			names = append(names, n)
		}
		*l.list = names
	}
	return nil
}
