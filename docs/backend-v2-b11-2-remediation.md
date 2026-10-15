# B11.2 — контролируемое исправление configuration drift

B11.2 закрывает цепочку `обнаружить → предложить → отдельно подтвердить → выполнить → перечитать → проверить`. Код Agent и Collector не меняется. Изменение выполняет отдельный Linux Automation Worker из B10 через SSH.

## Что разрешено

Backend поддерживает ровно один action: возврат одного физического access-порта Cisco IOS или Arista EOS в VLAN из approved baseline. Запрос не содержит команд, playbook или configuration text. Backend сам берёт interface и expected VLAN из текущего B11.1 drift result. VyOS, trunk, routed port, Port-Channel, несколько отличий и неизвестная конфигурация остаются только диагностикой.

Worker применяет фиксированный vendor module `cisco.ios.ios_config` или `arista.eos.eos_config`, фиксированные строки `switchport`, `switchport mode access`, `switchport access vlan N`, сохраняет изменённую конфигурацию и в том же запуске перечитывает running config. Статус `verified` появляется только если новый snapshot сравнился с текущим baseline как `compliant`.

## Политика

Администратор сначала создаёт policy конкретного Asset:

```http
PUT /api/assets/{asset_id}/configuration/remediation-policy
Authorization: Bearer ...
Content-Type: application/json

{
  "expected_revision": 0,
  "enabled": true,
  "allowed_vlans": [20, 40],
  "protected_interfaces": ["GigabitEthernet0/1"],
  "max_evidence_age_seconds": 900,
  "reason": "reviewed lab switch remediation boundary"
}
```

`expected_revision` защищает от потерянного обновления. Включённая policy обязана иметь хотя бы один разрешённый VLAN. Protected interface всегда запрещён, даже если VLAN разрешён. История policy хранится неизменно.

## Запрос и одобрение

Operator/admin создаёт заявку по свежему текущему drift:

```http
POST /api/assets/{asset_id}/configuration/remediations
Idempotency-Key: request-drift-42

{
  "drift_result_id": 42,
  "policy_revision": 1,
  "reason": "restore reviewed access VLAN"
}
```

Ответ `201` имеет состояние `pending_approval`. Затем live admin явно подтверждает точную revision:

```http
POST /api/configuration/remediations/{remediation_id}/approve
Idempotency-Key: approve-drift-42

{
  "expected_revision": 1,
  "confirmed": true,
  "timeout_seconds": 90,
  "reason": "checked port, VLAN and maintenance window"
}
```

Ответ `202` означает только `queued`. Он не обещает успех. Состояния: `pending_approval → queued → running → verified`; ошибки дают `failed` или `cancelled` с фиксированным `error_code`.

Requester или admin может отменить активную заявку через `POST /api/configuration/remediations/{id}/cancel`, передав точную `expected_revision` и reason. Pending/queued заявка сразу получает `cancelled`; running заявка сначала получает `cancel_requested`, worker останавливается и только затем фиксируется `cancelled`.

## Проверки перед изменением

При request, approval, worker claim и во время выполнения проверяются:

- Asset всё ещё observed, не merged и не scoped;
- Asset revision, automation target revision/template и approved baseline не менялись;
- drift result остаётся самым новым для текущего baseline;
- evidence не старше policy max age и не находится в будущем;
- action по-прежнему единственный безопасно распознанный access VLAN drift;
- VLAN есть в allowlist, interface отсутствует в protected list;
- target включён, worker совместим и пользовательская session/role действуют.

Любая смена policy, target, baseline, Asset, evidence или прав до/во время запуска останавливает выполнение. Все request/approval/policy/worker transitions остаются в PostgreSQL и access audit.

## Запуск

Backend сначала применяет migration 034. Затем перезапустите новый Automation Worker: старый B10/B11.1 worker не знает write action. Сохраняйте прежний `AUTOMATION_ENCRYPTION_KEY`, credential files, narrow `AUTOMATION_ALLOWED_CIDRS` и pinned SSH host key. Для demo policy включайте только на выбранном лабораторном Asset, с одним VLAN и без uplink-портов.

Реальную проверку делайте на эмуляторе или выделенном switch: вручную измените один access VLAN, дождитесь свежего drift, создайте request, одобрите, дождитесь `verified`, затем проверьте running config и новый drift result. До такой проверки нельзя заявлять production-ready remediation.
