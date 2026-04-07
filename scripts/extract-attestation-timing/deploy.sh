#!/bin/bash
set -euo pipefail

# Deploy attestation timing extraction jobs across 4 genpop nodes.
# Uses inline Python script in a debian container - no Docker image needed.
# Each worker uses local-path storage (~4 GB output each).
#
# Output: ~17 MB/day, ~13 GB total for Jan 2024 - Feb 2026
# Time: ~16 min/day per worker, ~24 hours per worker
#
# Output files are idempotent (skip existing).

CONTEXT="platform-analytics-hel1-staging"
NAMESPACE="panda"

# Jan 2024 - Feb 2026 (~790 days), split across 4 workers
declare -A WORKERS
WORKERS[0]="2024-01-01,2024-07-31"
WORKERS[1]="2024-08-01,2025-02-28"
WORKERS[2]="2025-03-01,2025-09-30"
WORKERS[3]="2025-10-01,2026-02-28"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Deploying attestation timing extraction jobs"
echo "Context: $CONTEXT"
echo "Namespace: $NAMESPACE"
echo ""

# Create ConfigMap with the extract script
echo "Creating ConfigMap with extract script..."
kubectl create configmap fcr-extract-script \
    --from-file=extract.py="$SCRIPT_DIR/extract.py" \
    --context "$CONTEXT" -n "$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply --context "$CONTEXT" -n "$NAMESPACE" -f -

for worker_id in "${!WORKERS[@]}"; do
    IFS=',' read -r start_date end_date <<< "${WORKERS[$worker_id]}"
    job_name="fcr-extract-worker-${worker_id}"
    pvc_name="fcr-extract-data-${worker_id}"

    echo ""
    echo "Worker $worker_id: $start_date to $end_date ($job_name)"

    kubectl delete job "$job_name" --context "$CONTEXT" -n "$NAMESPACE" --ignore-not-found 2>/dev/null

    # Create per-worker PVC with local-path
    kubectl apply --context "$CONTEXT" -n "$NAMESPACE" -f - <<PVCEOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${pvc_name}
  namespace: ${NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
  storageClassName: local-path
PVCEOF

    kubectl apply --context "$CONTEXT" -n "$NAMESPACE" -f - <<JOBEOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${NAMESPACE}
spec:
  backoffLimit: 1
  activeDeadlineSeconds: 259200
  template:
    spec:
      restartPolicy: Never
      nodeSelector:
        node.kubernetes.io/pool: dedicated-genpop
      volumes:
        - name: output
          persistentVolumeClaim:
            claimName: ${pvc_name}
        - name: script
          configMap:
            name: fcr-extract-script
      containers:
        - name: extract
          image: python:3.11-slim
          env:
            - name: PYTHONUNBUFFERED
              value: "1"
          command:
            - /bin/bash
            - -c
            - |
              pip install --no-cache-dir duckdb httpx && \
              python3 -u /scripts/extract.py \
                --start-date "${start_date}" \
                --end-date "${end_date}" \
                --output-dir /output/mainnet/v1
          volumeMounts:
            - name: output
              mountPath: /output
            - name: script
              mountPath: /scripts
          resources:
            requests:
              cpu: "2"
              memory: "16Gi"
            limits:
              cpu: "10"
              memory: "48Gi"
JOBEOF

    echo "  Deployed $job_name"
done

echo ""
echo "All workers deployed. Monitor with:"
echo "  kubectl get jobs -n $NAMESPACE --context $CONTEXT | grep fcr-extract"
echo "  kubectl logs -f job/fcr-extract-worker-0 -n $NAMESPACE --context $CONTEXT"
echo ""
echo "When done, copy output from each worker:"
for worker_id in "${!WORKERS[@]}"; do
    echo "  kubectl cp $NAMESPACE/\$(kubectl get pods -n $NAMESPACE --context $CONTEXT -l job-name=fcr-extract-worker-${worker_id} -o jsonpath='{.items[0].metadata.name}'):/output/mainnet/v1 ./attestation-timing/mainnet/v1/ --context $CONTEXT"
done
