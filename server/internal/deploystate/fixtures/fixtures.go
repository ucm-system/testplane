// Package fixtures 提供 deploy-state.json 的**样例字节**（v2 取代 v1 的
// genet/mocks 假 Deployer——平台不再直连 Genet（D-70），没有需要 mock 的
// 远端接口，需要的是 service-deploy runner 产物的静态形态）。
//
// 用途：
//   - deploystate 单测（phase 全谱 + blocked/timeout 归类用例）；
//   - UI / E2E 的静态 deploy-state 样本（不启动 genet-local 也能渲染看板）；
//   - 内置 service-deploy runner 波次的输出对照（golden）。
//
// 每个样例的形状以 design/impl/execution.md §9.3 第 3 步的 JSON 契约为准。
package fixtures

// Queued 返回 phase=queued 的样例：Genet 已受理但 Pod 未调度（排队中，
// 水位有、位次无，D-46）。
func Queued() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "pending-install",
  "endpoint_status": "pending",
  "phase": "queued",
  "reason": "PodGroup Pending: 0/1 pods are ready, insufficient resources",
  "waited_seconds": 4210,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [
    { "name": "arg-9f3a1c2b7d40-engine-0", "role": "engine", "node": "", "restarts": 0, "ready": false }
  ],
  "queue": { "phase": "Pending", "pending": 3, "inqueue": 1, "running": 2, "position": null },
  "exports": null,
  "failure": null
}`)
}

// Accepted 返回 phase=accepted 的样例：release 已出现但 Pod 还没起来
// （404 宽限窗内的恢复路径，§9.3 第 2 步）。
func Accepted() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "pending-install",
  "endpoint_status": "pending",
  "phase": "accepted",
  "reason": "release visible, no pod scheduled yet",
  "waited_seconds": 95,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [],
  "queue": { "phase": "Inqueue", "pending": 0, "inqueue": 1, "running": 2, "position": null },
  "exports": null,
  "failure": null
}`)
}

// Deploying 返回 phase=deploying 的样例：release deployed 但 endpoint 仍 pending
// （deployed + endpoint.status=pending 不可当 ready，§9.3 第 4 步）。
func Deploying() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "deployed",
  "endpoint_status": "pending",
  "phase": "deploying",
  "reason": "release deployed but callInfo.endpoint is still pending",
  "waited_seconds": 640,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [
    { "name": "arg-9f3a1c2b7d40-engine-0", "role": "engine", "node": "node-a", "restarts": 1, "ready": false },
    { "name": "arg-9f3a1c2b7d40-router-0", "role": "router", "node": "node-b", "restarts": 0, "ready": false }
  ],
  "queue": { "phase": "Running", "pending": 0, "inqueue": 0, "running": 3, "position": null },
  "exports": null,
  "failure": null
}`)
}

// Ready 返回 phase=ready 的样例：服务就绪，exports 四件套齐全
// （AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH / AG_DEPLOY_REF，§9.3 第 5 步）。
func Ready() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "deployed",
  "endpoint_status": "ready",
  "phase": "ready",
  "reason": "endpoint ready (address resolved; availability still needs pre-check)",
  "waited_seconds": 1810,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [
    { "name": "arg-9f3a1c2b7d40-engine-0", "role": "engine", "node": "node-a", "restarts": 1, "ready": true },
    { "name": "arg-9f3a1c2b7d40-router-0", "role": "router", "node": "node-b", "restarts": 0, "ready": true }
  ],
  "queue": { "phase": "Running", "pending": 0, "inqueue": 0, "running": 3, "position": null },
  "exports": {
    "AG_SERVED_URL": "http://10.0.0.10:8000/v1",
    "AG_SERVED_MODEL_NAME": "Qwen3-32B",
    "AG_MODEL_PATH": "/models/Qwen3-32B",
    "AG_DEPLOY_REF": "genet-ns-01/arg-9f3a1c2b7d40"
  },
  "failure": null
}`)
}

// Blocked 返回 phase=blocked 的样例：Genet 终态失败（helm release failed，
// D-47 折叠），failure.code=DEPLOY_BLOCKED。
func Blocked() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "failed",
  "endpoint_status": "blocked",
  "phase": "blocked",
  "reason": "release failed: install chart timeout",
  "waited_seconds": 9000,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [
    { "name": "arg-9f3a1c2b7d40-engine-0", "role": "engine", "node": "node-a", "restarts": 0, "ready": false }
  ],
  "queue": null,
  "exports": null,
  "failure": {
    "code": "DEPLOY_BLOCKED",
    "reason": "blocked",
    "message": "release failed: install chart timeout"
  }
}`)
}

// BlockedTimeout 返回超部署窗口的样例：phase=blocked + failure.reason=timeout
// （默认 72h，AG_DEPLOY_TIMEOUT；ARCHITECTURE §7.4）。
func BlockedTimeout() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "pending-install",
  "endpoint_status": "pending",
  "phase": "blocked",
  "reason": "timeout",
  "waited_seconds": 259200,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [],
  "queue": { "phase": "Pending", "pending": 8, "inqueue": 2, "running": 4, "position": null },
  "exports": null,
  "failure": {
    "code": "DEPLOY_BLOCKED",
    "reason": "timeout",
    "message": "deploy did not become ready within the deploy window"
  }
}`)
}

// CallFailed 返回 Genet 调用失败的样例：failure.code=DEPLOY_FAILED
// （连接/5xx/鉴权/同名冲突；平台五值没有 failed 子态，phase 折叠为 blocked，D-47）。
func CallFailed() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "",
  "endpoint_status": "",
  "phase": "blocked",
  "reason": "genet_call_failed",
  "waited_seconds": 12,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [],
  "queue": null,
  "exports": null,
  "failure": {
    "code": "DEPLOY_FAILED",
    "reason": "genet_call_failed",
    "message": "POST /api/open/helm/uc-packages/stack/releases 401: invalid api key"
  }
}`)
}

// Malformed 返回非法 JSON（Parse 必须报错且调用方"下轮重读"，§9.5）。
func Malformed() []byte {
	return []byte(`{"release_name": "arg-9f3a1c2b7d40", "phase": `)
}

// BadPhase 返回 phase 不在五值枚举内的样例（结构校验必须拒绝）。
func BadPhase() []byte {
	return []byte(`{
  "release_name": "arg-9f3a1c2b7d40",
  "namespace": "genet-ns-01",
  "genet_status": "deployed",
  "phase": "running",
  "waited_seconds": 10
}`)
}

// Samples 是 phase 全谱 + 归类用例的命名集合（"正常五态" + 两个失败归类）。
// Malformed / BadPhase 不在其中——它们是负例，见 Negatives。
func Samples() map[string][]byte {
	return map[string][]byte{
		"queued":          Queued(),
		"accepted":        Accepted(),
		"deploying":       Deploying(),
		"ready":           Ready(),
		"blocked":         Blocked(),
		"blocked_timeout": BlockedTimeout(),
		"call_failed":     CallFailed(),
	}
}

// Negatives 是 Parse 必须拒绝的负例（名字 → 样例）。
func Negatives() map[string][]byte {
	return map[string][]byte{
		"malformed": Malformed(),
		"bad_phase": BadPhase(),
	}
}
