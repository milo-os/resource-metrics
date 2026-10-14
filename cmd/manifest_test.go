// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The Deployment passes the lease timing from environment variables, so each
// environment sets it without a release. These tests read the manifest the
// bundle ships and parse its lease arguments as the container receives them.

const managerManifest = "../config/manager/manager.yaml"

// envReference is a $(NAME) in a container argument, which the kubelet
// replaces with the container's variable NAME, or leaves as it is when the
// container sets no such variable.
var envReference = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)

func managerContainer(t *testing.T) corev1.Container {
	t.Helper()
	data, err := os.ReadFile(managerManifest)
	require.NoError(t, err)
	var deployment appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(data, &deployment))
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c
		}
	}
	t.Fatalf("%s has no container named manager", managerManifest)
	return corev1.Container{}
}

// leaseArgs returns the container's lease arguments with every variable
// replaced from env, and the names it could not replace.
func leaseArgs(c corev1.Container, env map[string]string) (args, missing []string) {
	for _, arg := range c.Args {
		expanded := envReference.ReplaceAllStringFunc(arg, func(ref string) string {
			name := envReference.FindStringSubmatch(ref)[1]
			value, ok := env[name]
			if !ok {
				missing = append(missing, name)
				return ref
			}
			return value
		})
		if strings.HasPrefix(expanded, "--leader-elect-") {
			args = append(args, expanded)
		}
	}
	return args, missing
}

func TestManifest_LeaseArgumentsParse(t *testing.T) {
	c := managerContainer(t)
	cases := map[string]struct {
		set  map[string]string
		want leaseTiming
	}{
		"the manifest's own values are the code's defaults": {
			want: leaseTiming{defaultLeaseDuration, defaultRenewDeadline, defaultRetryPeriod},
		},
		"an environment that sets longer values": {
			set: map[string]string{
				"LEADER_ELECT_LEASE_DURATION": "60s", "LEADER_ELECT_RENEW_DEADLINE": "40s", "LEADER_ELECT_RETRY_PERIOD": "5s",
			},
			want: leaseTiming{60 * time.Second, 40 * time.Second, 5 * time.Second},
		},
	}
	for name, tc := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			for k, v := range tc.set {
				env[k] = v
			}
			args, missing := leaseArgs(c, env)
			assert.Empty(t, missing, "the arguments name variables the container does not set")
			require.Len(t, args, 3, "the manifest does not pass the three lease flags")
			fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
			lease := bindLeaseFlags(fs)
			require.NoError(t, fs.Parse(args), "the manifest's lease arguments are not the code's flags")
			assert.Equal(t, tc.want, *lease)
		})
	}
}
