package access

import "testing"

func TestCommand(t *testing.T) {
	cases := []struct {
		tool, cmd, want string
	}{
		{"kube", `kubectl get pods 2>&1 | head -40`, Read},
		{"kube", `kubectl -n default get pods -o wide`, Read},
		{"kube", `kubectl --context gke_x logs deploy/api --tail 100 | grep ERROR`, Read},
		{"kube", `kubectl get pods; kubectl delete pod x`, Write},
		{"kube", `kubectl get pods | xargs -n1 kubectl delete pod`, Write},
		{"kube", `kubectl get secret db -o yaml`, Write},
		{"kube", `kubectl get secrets`, Write},
		{"kube", `echo $(kubectl delete pod x)`, Write},
		{"kube", `echo "$(kubectl get pods)"`, Read},
		{"kube", `kubectl get pods | python3 -c 'import sys'`, Write},
		{"kube", `bash -c 'kubectl get pods'`, Write},
		{"kube", `$K get pods`, Write},
		{"kube", `./deploy.sh`, Write},
		{"kube", `cd /x && kubectl get ns`, Read},
		{"kube", `KUBECONFIG=/x kubectl get ns`, Read},
		{"kube", `sudo -E kubectl get ns`, Read},
		{"kube", `env FOO=1 kubectl apply -f x.yaml`, Write},
		{"kube", `ls -la`, Write}, // doesn't use kubectl: can't attribute
		{"kube", ``, Write},
		{"kube", `kubectl exec -it pod -- sh`, Write},
		{"kube", `kubectl get pods > /tmp/out.txt 2>&1`, Read},
		{"kube", `diff <(kubectl get a) <(kubectl get b)`, Read},
		{"kube", `diff <(kubectl get a) <(kubectl delete b)`, Write},
		{"gcp", `for p in digital-pvp-prod digital-pvp; do echo "=== $p"; gcloud functions list --project $p --format='table(name.basename(),state)' 2>&1 | head -80; done`, Read},
		{"gcp", `gcloud run deploy api --source .`, Write},
		{"gcp", `gcloud auth print-access-token`, Write},
		{"gcp", `gcloud secrets versions access latest --secret x`, Write},
		{"gcp", `gcloud logging read 'severity>=ERROR' --limit 20`, Read},
		{"gcp", `bq query 'select 1'`, Write},
		{"aws", `aws --profile dev ec2 describe-instances --region eu-west-1`, Read},
		{"aws", `aws s3 ls s3://bucket`, Read},
		{"aws", `aws s3 cp x s3://bucket/x`, Write},
		{"aws", `aws secretsmanager get-secret-value --secret-id x`, Write},
		{"aws", `aws sts get-caller-identity`, Read},
		{"ssh", `cd /r; git fetch origin 2>&1 | tail -3`, Read},
		{"ssh", `git push -u origin feature/x 2>&1 | grep -v 'Operation not permitted' | tail -10`, Write},
		{"ssh", `ssh -T git@gitlab.com`, Write},
		{"ssh", `KH=$TMPDIR/kh; GIT_SSH_COMMAND="$GIT_SSH_COMMAND -o X" git -C /r fetch`, Read},
		{"git", `git ls-remote https://github.com/x/y`, Read},
		{"docker", `docker pull ghcr.io/x/y:1`, Read},
		{"docker", `docker push ghcr.io/x/y:1`, Write},
		{"docker", `docker image rm x`, Write},
		{"kube", `(latest Bash command) kubectl get pods`, Read}, // prefix stripped; callers decide trust
	}
	for _, c := range cases {
		got, why := Command(c.tool, c.cmd)
		if got != c.want {
			t.Errorf("%s %q: got %s (%s), want %s", c.tool, c.cmd, got, why, c.want)
		}
	}
}

func TestHTTPMethod(t *testing.T) {
	for m, want := range map[string]string{"GET": Read, "head": Read, "OPTIONS": Read, "POST": Write, "PUT": Write, "DELETE": Write, "PATCH": Write} {
		if got, _ := HTTPMethod(m); got != want {
			t.Errorf("%s: %s", m, got)
		}
	}
}
