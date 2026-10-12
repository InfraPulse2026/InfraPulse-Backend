# B11.2 — security policy

Unset `REMEDIATION_POLICY_FILE` означает disabled. Role operator/admin недостаточна:
нужен explicit user ID в local policy и exact Asset/binding/interface/VLAN rule.
Viewer может читать безопасное preview/history, но не создавать/approve/reject/reconcile.
Admin не обходит protection или allowlist. Existing cookie/Bearer auth, Origin/CSRF
и live PostgreSQL session/role checks сохранены, включая start и running checks.

Backend и worker загружают один bounded strict local JSON file на старте. Изменение
policy требует согласованного restart; несовпадающие pinned digests дают safe rejection.
Unknown JSON fields/trailing values/oversized/nonregular files и wildcard identities
отклоняются. `require_distinct_approver=true` дополнительно разделяет инициатора/approver.

Пример только для review, с placeholders вместо настоящих идентификаторов:

```json
{
  "enabled": true,
  "version": "lab_access_v1",
  "allowed_users": ["reviewed_operator_id"],
  "require_distinct_approver": false,
  "rules": [{
    "asset_id": "reviewed_asset_id",
    "binding_id": "reviewed_binding_id",
    "interface_id": "12",
    "interface": "GigabitEthernet0/12",
    "platform": "cisco.ios.ios",
    "role": "access",
    "protected": false,
    "allowed_vlans": [20, 30],
    "management_vlans": [1, 100]
  }]
}
```

Не копировать пример как production permission. Management VLAN list должна
отражать реальный reviewed switch; target/current management VLAN запрещены.
Нет fallback VLAN1. Технический диапазон 1–4094 не заменяет локальную allowlist.

## Asset и port identity

MVP требует real active Asset со canonical SourceBinding, enabled source,
Device.type SWITCH, vendor Cisco и recognized OS: `ios`, `ios-xe`, `Cisco IOS`,
`Cisco IOS XE` или `cisco.ios.ios` (case-insensitive). Vendor alone недостаточен;
NX-OS/unknown платформы отклоняются. Management IP должен совпасть с reviewed
B10 target canonical IP, SSH host key/CIDR/credential reference остаются existing B10.

Текущий wire Device не имеет списка interfaces. Поэтому порт берётся из свежего
existing TelemetrySample canonical binding legacy Agent/device pair, type6 Ethernet,
unique ID/full name и aliases. Внешний Asset без такого достоверного port evidence
безопасно отклоняется. Collector и protobuf не расширяются.

Binding fingerprint включает source/Agent routing keys, OS/model/software,
management address и relevant port identity. Clock/counters не являются action
parameters. Inventory/telemetry старше15 минут или future>30 секунд не подходят.

Existing `COMMAND_PROTECTED_DEVICES` и `COMMAND_PROTECTED_INTERFACES` применяются
по Asset, external/legacy/device IDs и port ID/name/alias/MAC. Port с management
address запрещён. Known topology uplink или unresolved local-port topology evidence
также запрещены; LLDP LocalPortNumber не трактуется как ifIndex.

В current schema нет универсального authoritative role/protected flag для Cisco.
`role=access` — explicit reviewed local attestation, дополненная fresh configuration
и registry vetoes. Нельзя принимать permission только по совпадению имени.

## Configuration grammar и исполнение

Expected и fresh actual должны иметь explicit `switchport mode access`, единственную
access VLAN строку и безопасную physical stanza. Поддерживается узкий набор scalar
команд: description/switchport/admin state/MTU/speed/duplex. Unknown/nested/duplicate,
voice, port-security, channel-group, trunk и routed commands запрещены. Это
намеренно ограниченный безопасный MVP; alias `Gi0/12` не заменяет full name.

Only server-owned `cisco_ios_restore_access_vlan_v1`, pinned по SHA-256 embedded
Runner adapter. Backend не принимает playbook/inventory paths, shell commands,
host expressions, module arguments, extra_vars или пользовательские template IDs.
Typed params реконструируются из immutable proposal; Ansible меняет одну access
VLAN line после собственного fresh guard.

Configuration/facts encrypted существующим Vault; public preview/diff/events
содержат IDs, digests, VLAN и safe codes. Credential values/private keys/tokens,
raw errors/Runner stdout/configuration в public API/audit/logs не копируются.
Полный snapshot читается только existing admin endpoint с audit.

## Crash и неопределённость

Persistent exclusive claim + deadlines, Asset serialization и PostgreSQL locks
работают между processes. DB transaction не держится во время device I/O.
Unknown не requeue; новую запись блокирует до explicit read-only reconciliation.
Reconciliation не подтверждает успех старого write, не подменяет approval и не
изменяет expected configuration. Новый write всегда проходит новый approval.

Remaining device TOCTOU не устраняется полностью: external administrators могут
менять устройство между Ansible check и operation. Правила этого MVP не являются
универсальным multi-vendor rollback или завершением B16.
