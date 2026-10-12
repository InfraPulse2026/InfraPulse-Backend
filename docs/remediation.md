# B11.2 — controlled access VLAN remediation

B11.2 завершает путь DETECT → PROPOSE → APPROVE → VALIDATE → EXECUTE → VERIFY → AUDIT
для одного Cisco IOS / IOS-XE access-порта. B11.1 comparator и B10 Ansible Runner,
worker, credential store, jobs, events и encrypted snapshots переиспользуются.
Frontend, Agent/Collector, protobuf и Incident rules не изменяются.

**Deployment/merge заблокирован номером миграции:** SQL находится в
`internal/storage/pending_migrations/NEEDS_NUMBER_COORDINATION_b11_2.sql`,
не включён в `Store.Migrate`. Не считать номер 034 зарезервированным. Команда
должна согласовать номер, перенести SQL в numbered migrations и добавить его
embed/application/schema guard по existing conventions. Старые миграции не правятся.
Enabled startup без schema отклоняется; disabled B10/B11.1 продолжают работать.

## Архитектура

1. Existing drift должен быть последним, свежим и содержать ровно одно безопасное
   access VLAN расхождение. Expected остаётся immutable approved baseline.
2. Backend сохраняет concrete proposal: Asset/revision, canonical binding/digest,
   target/revision/digest, port ID/full name, drift/baseline/snapshot IDs, normalized
   expected/actual digests, current/target VLAN, write template/digest и policy digest.
3. SHA-256 fixed-field versioned JSON fingerprint охватывает все action fields.
   Database trigger запрещает изменение action, digest, preview, автора и expiry.
4. Human approval требует `confirmed=true` и точный digest уже сохранённого preview.
   Approval identity/request ID и единственный existing AutomationJob фиксируются
   атомарно. Action parameters из approve body не принимаются.
5. Worker делает отдельный fresh read-only gather. В коротких DB transactions
   повторно проверяет live role/session, policy, source/target/port identity,
   baseline и expected digest. Existing comparator должен подтвердить прежний drift.
6. При unchanged action запускается только `cisco_ios_restore_access_vlan_v1`.
   В workflow ещё раз читается точный порт, проверяется access/current VLAN/grammar,
   затем `cisco.ios.ios_config` меняет одну строку access VLAN.
7. Backend отдельно повторяет read-only gather и `configdrift.Compare`. Проверяются
   target VLAN, безопасный прежний mode/role, unchanged baseline/binding/policy и
   отсутствие других normalized configuration изменений. Только затем `succeeded`.

Job имеет write template ID; target сохраняет read template ID. Это предотвращает
claim remediation старым B10 worker, знающим только read templates. Новая версия
worker рекламирует pinned restore digest. API B10 `Templates/Lookup/TargetInput`
остаётся read-only: обычный CreateAutomation не может выбрать write template.
Backend и workers нужно обновить вместе и дать им одну reviewed policy.

## Lifecycle и concurrency

`proposed → queued → running → verifying → succeeded | verification_failed | unknown`.
Возможны `rejected`, `expired`, `stale`, `no_action`, `cancelled`, `failed`.
Approval сразу ставит asynchronous job в очередь; HTTP 202 не означает применение.
Если fresh state уже compliant — `no_action`, без write. Изменившийся drift,
mode, expected revision, target, source, policy или protection даёт `stale`.

`UNIQUE(created_by,idempotency_key)` защищает creation. Proposal row lock и unique
same-Asset job relationship защищают concurrent approval; повтор того же approval
тем же пользователем возвращает тот же job. Другой approver получает conflict.
Existing partial unique active-Asset index распространяется на verifying: один
active job на Asset, даже для разных портов. DB locks держатся только на checks
и persistence; через SSH PostgreSQL transaction не удерживается.

| Предел | Значение |
|---|---|
| Pending proposals | 100 |
| Outstanding jobs | 100 |
| Concurrent Automation jobs при B11.2 schema | 4 |
| Approval expiry | 5 минут от creation |
| Queue wait | 1 минута |
| Полный execution workflow | 120 секунд |
| Каждый gather | 30 секунд |
| SSH connect/command | Existing B10 15/30 секунд |
| Configuration / facts | 4 MiB / 128 KiB на snapshot |
| History page | 1–200, offset ≤100000 |
| Local policy file | 64 KiB |

Worker heartbeat/claim сверяет expired queues и orphan jobs даже при unavailable
Runner. Когда все workers выключены, persistence сверяется после их возвращения;
никакая просроченная очередь при этом не получает write. Автоматических повторов
изменений нет. Temporary Runner artifacts очищаются существующим Process;
исторические DB snapshots/audit сохраняются, общая DB retention остаётся existing
операционной политикой (B16 hardening не выполняется).

## Неизвестный исход и ручная сверка

Timeout, cancellation после dispatch, lost result/worker и недостоверная verification
дают `unknown`; это не утверждение, что write не произошёл. Unknown блокирует новую
remediation для Asset. Автоматический retry запрещён; B10 read-only gather разрешён.

Авторизованный оператор ждёт original deadline +35 секунд, запускает отдельный B10
read-only job и явно вызывает `/reconcile` с его новым successful snapshot. Snapshot
должен быть получен после persistence UNKNOWN и deadline grace, иметь current
same-Asset/target identity и быть свежим. Старые expected/actual snapshots и snapshots
write jobs не подходят. Existing comparator сверяет current approved baseline.

Proposal становится `reconciled`; original AutomationJob остаётся `unknown`,
`original_execution_unknown` сохраняет неопределённость исходного write. Это
read-back evidence, а не retroactive success. Если drift остался, нужны новое
proposal и новый human approval. Поздние/повторные callbacks старого job отклоняются.

## История и границы

Original drift record не удаляется. Action ссылается на original actual/expected;
live preflight и after snapshots хранятся encrypted с phases preflight/result.
Verification сохраняет sanitized comparator и новый drift record ID. Existing
current drift projection показывает actual после исправления без изменения expected.
After snapshots jobs=`unknown/verification_failed` не годятся для baseline approval;
последующая работа использует отдельный successful B10 gather.

Existing `access_audit` связывает approver/proposal/job/request и result; existing
automation events показывают phases. Denied policy attempts также аудируются,
без reasons/raw config/secrets. Никакое восстановление VLAN не закрывает общий Incident.
AI не получает approval или execution permissions.

Сетевое устройство не предоставляет общей conditional transaction с PostgreSQL.
Fresh backend gather и immediate Ansible guard сокращают TOCTOU, но не обеспечивают
абсолютную атомарность. Изменяется только running configuration, `save_when=never`;
startup config, reboot/reset, trunks/routes/ACL/credentials и generic playbooks вне MVP.

## Лабораторная real-device приёмка

Реальный write в этой разработке не выполнялся. Для приёмки:

1. Получить разрешение владельца test switch, определить точную IOS/IOS-XE модель,
   firmware и один незащищённый физический access-порт с full canonical name.
2. Проверить out-of-band доступ, VLAN allowlist, management VLAN denylist и rollback
   штатным способом устройства. Не выбирать management/uplink/trunk/LAG порт.
3. Обновить backend/worker, применить только согласованную миграцию. Настроить B10
   secure credential ref с минимально нужными laboratory read/write privileges,
   pinned SSH host key и narrow CIDR; credentials вне Git/API/logs.
4. Проверить canonical SourceBinding, recognized IOS OS evidence и свежую реальную
   Collector telemetry для точного Interface ID/type/aliases. Reviewed policy
   должна перечислять этот Asset/binding/port, user ID и VLAN, `role=access`.
5. Получить known-good VLAN20 snapshot B10 и явно утвердить baseline B11.1.
   Владелец стенда штатно меняет только этот access VLAN на разрешённый VLAN30.
6. Получить actual snapshot, проверить drift, создать и просмотреть immutable preview,
   выполнить один explicit approval. Ожидать asynchronous job и independent read-back.
7. Проверить VLAN20 на устройстве и API verification/snapshots/drift/audit; duplicate
   approval должен вернуть прежний job. Повторить отдельный stale-expected сценарий:
   после approval сменить baseline; write не должен произойти.

Без этих условий real Linux Runner/SSH E2E имеет статус NOT EXECUTED.

Связанные документы: [security](remediation-security.md), [frontend/API handoff](remediation-api.md),
[validation report](b11-2-report.md), [preimplementation design](remediation-design.md).
Template module: [официальный ios_config](https://docs.ansible.com/projects/ansible/latest/collections/cisco/ios/ios_config_module.html).
