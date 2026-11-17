package cloudenv

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mechanomythia/iris/internal/provision"
)

func fakeRunner(resp map[string]string) (*provision.Runner, *[]string) {
	var calls []string
	r := &provision.Runner{Out: &bytes.Buffer{}, Exec: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		argv := name + " " + strings.Join(args, " ")
		calls = append(calls, argv)
		// gcloud keys create writes the key to the path given
		if strings.Contains(argv, "keys create") {
			os.WriteFile(args[4], []byte(`{"type":"service_account"}`), 0o600)
		}
		for p, out := range resp {
			if strings.HasPrefix(argv, p) {
				if out == "ERR" {
					return nil, errors.New("not found")
				}
				return []byte(out), nil
			}
		}
		return nil, nil
	}}
	return r, &calls
}

func TestCreateGCP(t *testing.T) {
	r, calls := fakeRunner(map[string]string{"gcloud iam service-accounts describe": "ERR"})
	email, key, err := CreateGCP(context.Background(), r, full())
	if err != nil {
		t.Fatal(err)
	}
	if email != "claude-acme-api@acme-prod.iam.gserviceaccount.com" || key != "eyJ0eXBlIjoic2VydmljZV9hY2NvdW50In0=" {
		t.Fatalf("%s %s", email, key)
	}
	all := strings.Join(*calls, "\n")
	for _, w := range []string{"service-accounts create claude-acme-api --project acme-prod",
		"add-iam-policy-binding acme-build --member serviceAccount:claude-acme-api@acme-prod.iam.gserviceaccount.com --role roles/artifactregistry.writer",
		"--role roles/container.viewer", "keys create"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in\n%s", w, all)
		}
	}
}

func TestCreateAWS(t *testing.T) {
	r, calls := fakeRunner(map[string]string{
		"aws iam get-user":          "ERR",
		"aws iam create-access-key": `{"AccessKey":{"AccessKeyId":"AKIAX","SecretAccessKey":"sek"}}`,
	})
	user, id, sec, err := CreateAWS(context.Background(), r, full(), "admin")
	if err != nil || user != "iris-claude-acme-api" || id != "AKIAX" || sec != "sek" {
		t.Fatal(err, user, id, sec)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "attach-user-policy --user-name iris-claude-acme-api --policy-arn arn:aws:iam::aws:policy/ReadOnlyAccess --profile admin") {
		t.Fatal(*calls)
	}
}

func TestDryRunCreatesNothing(t *testing.T) {
	r, calls := fakeRunner(map[string]string{"gcloud iam service-accounts describe": "ERR", "aws iam get-user": "ERR"})
	r.DryRun = true
	CreateGCP(context.Background(), r, full())
	CreateAWS(context.Background(), r, full(), "")
	for _, c := range *calls {
		if strings.Contains(c, " create") || strings.Contains(c, "add-iam-policy-binding") || strings.Contains(c, "attach-user-policy") {
			t.Fatalf("dry-run executed a mutation: %s", c)
		}
	}
}
