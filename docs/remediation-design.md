# B11.2 — решение перед реализацией

База: `b99b26e`, ветка `feat/backend-b11-2-remediation`. Scope только Backend.

## Переиспользуемые компоненты

- B11.1: immutable baseline revisions, encrypted expected/actual snapshots,
  `configdrift.Compare`, stored drift history and conservative VLAN suggestion.
- B10: `automation_jobs`, claim ownership, worker heartbeat, Executor/Process,
  embedded Ansible Runner, secure credentials/CIDR/SSH host key validation,
  encrypted snapshots, job events and audit.
- Existing cookie/Bearer authorization, CSRF, live database role/session checks,
  Asset Registry/SourceBinding and protected device/interface rules.

## Конкретное действие

MVP: только Cisco IOS `restore_access_vlan` на одном полном физическом interface
name. Идентичность порта проверяется по ID и имени в canonical SourceBinding,
а reviewed local policy явно задаёт role=access. Unknown/trunk/LAG/management,
наблюдаемый topology uplink и protected aliases отклоняются. Target VLAN должен
быть в allowlist; отсутствующие значения не превращаются в VLAN 1.

Сохранённое proposal содержит exact Asset/binding/target/interface identity,
baseline revision, drift и snapshot IDs, normalized digests, current/target VLAN,
закреплённый write template/digest и policy digest. Детерминированный SHA-256
связывает все параметры. Database trigger запрещает их изменение.

## Approval и исполнение

API создания принимает только saved drift ID и reason. Approve принимает
confirmed, digest сохранённого preview и reason; action parameters не принимаются.
Текущий operator/admin дополнительно должен находиться в local policy allowlist.
Admin не обходит protected resources. Approval и единственная AutomationJob
создаются в одной короткой PostgreSQL-транзакции. Повтор возвращает ту же job.

Worker получает свежий read-only snapshot существующим Executor, повторно
проверяет identity/session/policy/baseline/digest и B11.1 comparator. Если drift
исчез — no_action; если изменился — stale, без write. Typed Runner workflow
повторяет relevant-port precondition непосредственно перед единственным
`ios_config` access VLAN change. Затем Backend отдельно вызывает read-only
gather и existing comparator. Exit code 0 без read-back не означает success.

## Persistent lifecycle и пределы

Proposal: proposed → queued → running → verifying → succeeded,
verification_failed, unknown, stale, no_action, rejected, expired, cancelled.
Approval одноразовый, TTL 5 минут; queue wait 1 минута; execution 120 секунд;
каждый gather 30 секунд; pending ≤100, active queue ≤100, remediation concurrency
≤4. Existing per-Asset serialization сильнее per-interface и сохраняется.

Before/after snapshots зашифрованы; API показывает только IDs и safe comparison.
Все события используют existing job events/access audit. Incident не закрывается.
Unknown outcome не requeue; оно блокирует новый write для того же Asset.
Existing B10 read-only gather остаётся доступным для ручной проверки.

## Миграция и контракт

Нужна одна proposal table и additive extension existing job states/snapshot phases.
Номер после 033 пока не согласован. SQL будет подготовлен вне automatically
applied migrations как `NEEDS_NUMBER_COORDINATION`; enabled startup требует
явно установленную schema. Merge/deployment readiness до координации отсутствует.

Минимальные routes: create from Asset drift, GET proposal/preview/status/evidence,
approve, reject, paginated history. Existing routes сохраняются. OpenAPI minor
increment соответствует практике B10/B11.1. Generic commands, extra_vars,
playbook/inventory paths и AI execution endpoints не добавляются.

## Проверки и ограничения

Baseline build/vet/module checks и existing Python fixture прошли; полные
PostgreSQL/race suites выполняются до существенных изменений. Дальше: domain,
RBAC/CSRF/input, stale/denied/no-write, PostgreSQL concurrency/immutability/recovery,
fake Runner DETECT→APPROVE→EXECUTE→VERIFY→AUDIT, regression/race/OpenAPI.
Linux Runner/device E2E требует отдельного стенда и разрешения владельца.

Сетевое устройство не предоставляет общей DB-транзакции: remaining TOCTOU
между device precondition и write документируется. Running config изменяется
без save/reboot; startup configuration remediation вне MVP.
