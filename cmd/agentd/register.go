package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ordinalFromHostname extracts the StatefulSet ordinal from a pod name
// ("<statefulset>-<ordinal>"). Returns 0 when there is no numeric suffix.
func ordinalFromHostname(host string) int {
	i := strings.LastIndexByte(host, '-')
	if i < 0 || i == len(host)-1 {
		return 0
	}
	n, err := strconv.Atoi(host[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// fetchRegistration, when RUNTIME_REGISTRATION_URL and _TOKEN are both set,
// POSTs to the control plane and os.Setenv's every returned pair into this
// process's environment, BEFORE the normal os.Getenv startup path runs. A no-op
// when either var is unset (local spawns are byte-for-byte unchanged). Fails
// hard (log.Fatal) on any error — a pod that cannot fetch its config must not
// start with a partial environment; K8s will restart it.
func fetchRegistration() {
	url := os.Getenv("RUNTIME_REGISTRATION_URL")
	token := os.Getenv("RUNTIME_REGISTRATION_TOKEN")
	if url == "" || token == "" {
		return
	}
	ordinal := ordinalFromHostname(os.Getenv("HOSTNAME"))
	reqBody, _ := json.Marshal(map[string]int{"ordinal": ordinal})
	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		log.Fatalf("agentd: build registration request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("agentd: registration handshake to %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("agentd: registration handshake to %s: status %s", url, resp.Status)
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Fatalf("agentd: decode registration response: %v", err)
	}
	for k, v := range out.Env {
		// Empty-valued delta entries have two distinct meanings:
		//
		//   1. Platform TOGGLE keys (platformToggleKeys) are control-plane-owned and
		//      emitted "always-explicit" in the spawn env (controlplane/proxy.go
		//      envDelta): a non-empty value turns the feature on, an EMPTY value means
		//      "off". On the remote path the empty signal MUST clear any inherited/
		//      leaked/operator value (os.Unsetenv) so it can't silently enable the
		//      feature on a remote pod — e.g. a leaked RUNTIME_SUBJECT_FORWARDING=1
		//      would otherwise let a caller spoof the forwarded subject.
		//
		//   2. All OTHER keys keep skip-empty: for a REMOTE agent the envDelta emits
		//      RUNTIME_LISTEN_ADDR="" and DBOS__VMID="" (it sets BaseURL, not
		//      Addr/DBOSVMID). The bind address and executor id are operator/pod-
		//      provided infra config (the chart sets RUNTIME_LISTEN_ADDR=":8080"
		//      statically). Applying the empty value would clobber that infra config
		//      and agentd could not bind, so an empty value means "not provided" —
		//      leave the inherited var.
		if v == "" {
			if platformToggleKeys[k] {
				if err := os.Unsetenv(k); err != nil {
					log.Fatalf("agentd: clear registration env %s: %v", k, err)
				}
			}
			// non-toggle empty ⇒ leave inherited (infra config); skip.
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			log.Fatalf("agentd: apply registration env %s: %v", k, err)
		}
	}
}

// platformToggleKeys are control-plane-owned toggles emitted "always-explicit"
// in the spawn env (controlplane/proxy.go envDelta): a non-empty value turns the
// feature on, an EMPTY value means "off — clear any inherited/operator value so
// it can't silently enable the feature on a remote pod". For these keys empty
// ⇒ Unsetenv (not skip). All other keys keep skip-empty so genuinely
// infra-provided vars (RUNTIME_LISTEN_ADDR, DBOS__VMID) are not clobbered.
var platformToggleKeys = map[string]bool{
	"RUNTIME_SUBJECT_FORWARDING": true,
	"RUNTIME_AGENT_MEMORY":       true,
	"RUNTIME_GATEWAY_URL":        true,
	"RUNTIME_GATEWAY_KEY":        true,
}
