package proxmox

import (
	"errors"
	"strings"
)

// CheckToken says what is wrong with an API token's shape before any call
// is made, without quoting it. Proxmox takes user@realm!tokenid=secret,
// with or without the "PVEAPIToken=" prefix; the secret alone, or the token
// id without its secret, is refused by the cluster with a bare 401.
func CheckToken(token string) error {
	t := strings.TrimPrefix(token, "PVEAPIToken=")
	at := strings.Index(t, "@")
	bang := strings.Index(t, "!")
	eq := strings.Index(t, "=")
	if at < 1 || bang < at+2 || eq < bang+2 || eq == len(t)-1 {
		return errors.New("the token is not user@realm!tokenid=secret: Proxmox needs the token's id and its secret together, as in ops@pve!accrete=<secret>")
	}
	return nil
}

// Explain returns what an operator should check for an error the cluster
// answered with, or "" when there is nothing to add to the error itself.
func Explain(err error) string {
	var api *APIError
	if !errors.As(err, &api) {
		return ""
	}
	switch api.Code {
	case 401:
		return "the cluster refused the token: check its id, its secret and that it has not expired"
	case 403:
		return "the token lacks a privilege for this read"
	}
	return ""
}
