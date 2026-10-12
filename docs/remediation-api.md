# B11.2 — frontend handoff / OpenAPI 0.33.0

Existing B10/B11.1 endpoints сохранены. B11.1 recommendation остаётся diagnostic;
новая persisted remediation создаётся отдельно от latest drift record ID.
Default disabled/missing coordinated schema возвращает503.

| Operation | Body / ответ |
|---|---|
| POST `/api/assets/{id}/configuration/remediations` | `drift_id`, `reason`; обязательный Idempotency-Key;201 proposal+Location |
| GET `/api/remediations/{id}` | Concrete immutable preview, state, optional job/verification evidence |
| POST `/api/remediations/{id}/approve` | `confirmed:true`, exact `digest`, `reason`;202 proposal+status Location |
| POST `/api/remediations/{id}/reject` | `reason`;200 rejected proposal |
| GET `/api/remediations/{id}/history?limit=50&offset=0` | Existing sanitized audit events in items; limit1–200/offset≤100000 |
| POST `/api/remediations/{id}/reconcile` | New successful B10 read-only `snapshot_id`, `confirmed:true`, `reason`;200 read-back evidence |

Mutation roles operator/admin plus explicit local remediation capability for
creation/approval/rejection/reconcile. Viewer только GET. Cookie requests используют
existing CSRF header и Origin; Bearer auth действует как в остальных endpoints.
Unknown/duplicate query fields и arbitrary JSON fields не принимаются.

Creation example:

```json
{"drift_id": 123, "reason": "Review exact laboratory access VLAN drift"}
```

Approval example (digest брать только из полученного immutable proposal):

```json
{"confirmed": true, "digest": "<64 lowercase hex chars from preview>", "reason": "Approve VLAN20 on reviewed port"}
```

UI показывает `asset_name`, Action.AssetID/InterfaceID/Interface, current/target VLAN,
baseline revision/expected snapshot source, expected/actual observed dates,
template ID/digest, expiry и warnings. `confirmed=true` должен происходить после
конкретного пользовательского действия; diff view/AI analysis не является approval.
Action fields, target, VLAN, templates и extra_vars в approve body запрещены.

Повтор creation с тем же key/body возвращает прежний proposal; повтор approval тем
же approver/digest возвращает тот же job, `Idempotent-Replay:true`. Другой approver
получает409. После202 poll GET proposal; `running/verifying` не показывать как success.
Existing `/api/automation/jobs/{job_id}` и `/events` доступны для дополнительной истории.

Успех только `state=succeeded` + persisted `verification.status=compliant` и after
snapshot ID. `verification_failed` означает успешный read-back с несоответствием;
`unknown` — недостоверный исход. `no_action` — drift исчез до write. `stale` требует
нового read/drift/proposal/approval; параметры существующего proposal не редактируются.

При UNKNOWN предложить оператору отдельный B10 read-only job после original
deadline+35 секунд, затем explicit reconcile по новому snapshot. Snapshot должен
быть новее persistence UNKNOWN, свежим и current same-Asset/target. Old snapshots
не подходят. `reconciled` показывает current read-back; original job остаётся unknown.
Если drift остался, создание нового proposal/approval — отдельное пользовательское действие.

| HTTP | Meaning |
|---|---|
| 400 | Malformed/unknown JSON/query fields |
| 401 | Unauthenticated |
| 403 | Role/session/policy/protected denial |
| 404 | Resource отсутствует |
| 409 | Changed/expired action, conflict, used approval, unsafe reconciliation evidence |
| 422 | Typed action/input invalid |
| 503 | Disabled feature/missing schema/worker/capacity unavailable |

Новые endpoints не имеют отдельного429 limiter; queues/concurrency bounded, existing
authentication throttling сохраняется. Ошибки используют existing problem contract.
Никакие secrets/full configuration не требуются для этого UI.

OpenAPI validator также выявил20 inherited `required:[]` arrays: они удалены как
недопустимый OpenAPI3.0 syntax, сохраняя optional семантику прежних fields.
