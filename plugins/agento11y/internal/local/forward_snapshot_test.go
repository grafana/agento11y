package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
)

func TestForwardLoaderExtraSnapshots(t *testing.T) {
	clearForwardEnv(t)
	for _, primary := range []bool{false, true} {
		t.Run(fmt.Sprint("primary_", primary), func(t *testing.T) {
			lines := map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true"}
			if primary {
				lines["AGENTO11Y_ENDPOINT"] = "https://primary.example"
				lines["AGENTO11Y_AUTH_TENANT_ID"] = "primary-tenant"
				lines["AGENTO11Y_AUTH_TOKEN"] = "primary-token"
			}
			path := writeConfigEnvFile(t, lines)
			loader := newForwardLoader(path, nil)
			base := loader.load()
			count := len(base.destinations)
			extraPath := filepath.Join(filepath.Dir(path), "forwarding.toml")
			write := func(endpoint string) {
				t.Helper()
				require.NoError(t, os.WriteFile(extraPath, fmt.Appendf(nil, "[[destinations]]\nname = 'extra'\nendpoint = '%s'\ntenant_id = 'extra-tenant'\ntoken = 'extra-token'\n", endpoint), 0o600))
			}
			write("https://first.example")
			first := loader.load()
			require.True(t, first.enabled)
			require.Len(t, first.destinations, count+1)
			require.Contains(t, first.destinations[count].config.genURL, "first.example")
			stat, err := os.Stat(extraPath)
			require.NoError(t, err)
			write("https://other.example")
			require.NoError(t, os.Chtimes(extraPath, stat.ModTime(), stat.ModTime()))
			require.Contains(t, loader.load().destinations[count].config.genURL, "other.example")
			require.Contains(t, first.destinations[count].config.genURL, "first.example")
			require.NoError(t, os.WriteFile(extraPath, []byte("token = 'secret-hostile-source'\ninvalid = ["), 0o600))
			invalid := loader.load()
			require.Len(t, invalid.destinations, count)
			require.NotEmpty(t, invalid.forwardingError)
			require.NotContains(t, invalid.forwardingError, "secret-hostile-source")
			require.Equal(t, base.genURL, invalid.genURL)
			require.NoError(t, os.Remove(extraPath))
			require.NoError(t, os.Mkdir(extraPath, 0o700))
			unreadable := loader.load()
			require.Len(t, unreadable.destinations, count)
			require.NotEmpty(t, unreadable.forwardingError)
			require.NoError(t, os.Remove(extraPath))
			write("https://first.example")
			require.Len(t, loader.load().destinations, count+1)
			require.NoError(t, os.Remove(extraPath))
			missing := loader.load()
			require.Len(t, missing.destinations, count)
			require.Empty(t, missing.forwardingError)
		})
	}
}

func TestForwardLoaderStaleExtraSnapshot(t *testing.T) {
	for _, change := range []string{"replace", "remove", "invalid", "add"} {
		for _, signal := range []string{"generations", "traces", "metrics"} {
			t.Run(change+"/"+signal, func(t *testing.T) {
				primary := newDestinationSink(t, "success")
				oldExtra := newDestinationSink(t, "success")
				newExtra := newDestinationSink(t, "success")
				sinks := []*destinationSink{primary}
				if change != "add" {
					sinks = append(sinks, oldExtra)
				}
				receiver := newDestinationReceiver(t, true, "full", true, sinks...)
				loader := receiver.forward
				old := loader.load()
				initial := loader.destinationStatuses(old)
				extraPath := filepath.Join(filepath.Dir(receiver.configPath), forwardingFile)
				switch change {
				case "remove":
					require.NoError(t, os.Remove(extraPath))
				case "invalid":
					require.NoError(t, os.WriteFile(extraPath, []byte("bad = ["), 0o600))
				default:
					writeDestinationSidecar(t, receiver.configPath, newExtra)
				}
				current := loader.load()
				require.Greater(t, current.snapshot, old.snapshot)
				body, path, label := destinationHTTPBatch(t, "generation"), wire.GenerationExportHTTPPath, forwardLabelGenerations
				if signal == "traces" {
					body, path, label = destinationTraceBatch(t, "generation"), "/v1/traces", forwardLabelOTLPGenerations
				} else if signal == "metrics" {
					body, path, label = destinationMetricsBatch(t), "/v1/metrics", otlpForwardLabel("metrics")
				}
				dispatch := func(cfg forwardConfig) {
					t.Helper()
					if signal == "generations" {
						var request generationsRequest
						require.NoError(t, json.Unmarshal(body, &request))
						loader.dispatchGenerations(cfg, request.Generations)
					} else {
						loader.dispatchOTLP(cfg, signal, wire.ContentTypeProto, "", body, label)
					}
					waitDispatch(t, loader)
				}
				dispatch(old)
				takeDestinationDelivery(t, primary, path)
				require.Empty(t, oldExtra.calls)
				require.Empty(t, newExtra.calls, "a new route must not receive a previously loaded batch")
				statuses := loader.destinationStatuses(old)
				require.Equal(t, initial[0].Revision, statuses[0].Revision)
				require.Equal(t, forwardLeg{Attempted: 1, Succeeded: 1, LastSuccessAt: statuses[0].Legs[label].LastSuccessAt}, statuses[0].Legs[label])
				require.NotEmpty(t, statuses[0].Legs[label].LastSuccessAt)
				if len(statuses) > 1 {
					require.Empty(t, statuses[1].Legs)
					if change == "replace" {
						require.NotEqual(t, initial[1].Revision, statuses[1].Revision)
					}
				}
				dispatch(current)
				takeDestinationDelivery(t, primary, path)
				if change == "replace" || change == "add" {
					takeDestinationDelivery(t, newExtra, path)
					require.EqualValues(t, 1, loader.destinationStatuses(current)[1].Legs[label].Succeeded)
				}
				require.Empty(t, oldExtra.calls)
				require.EqualValues(t, 2, loader.destinationStatuses(current)[0].Legs[label].Succeeded)
			})
		}
	}
}

func TestForwardLoaderExtraPolicy(t *testing.T) {
	clearForwardEnv(t)
	for _, tc := range []struct {
		name       string
		on         bool
		unreadable bool
	}{
		{name: "global_off"},
		{name: "global_on", on: true},
		{name: "unreadable_primary", on: true, unreadable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigEnvFile(t, map[string]string{"AGENTO11Y_LOCAL_FORWARD": fmt.Sprint(tc.on)})
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "forwarding.toml"), []byte("[[destinations]]\nname='extra'\nendpoint='https://extra.example'\ntenant_id='t'\ntoken='k'\n"), 0o600))
			if tc.unreadable {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			cfg := newForwardLoader(path, nil).load()
			require.Len(t, cfg.destinations, 1)
			require.Equal(t, tc.on && !tc.unreadable, cfg.forwarding)
			require.Equal(t, tc.on && !tc.unreadable, cfg.enabled)
		})
	}
}
