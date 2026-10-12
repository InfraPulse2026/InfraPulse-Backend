# Stage9F REST API compatibility matrix

Фиксация B0 от 2026-10-10. Существующие маршруты и их права сохраняются при переходе на v2. Новые Asset API добавляются рядом с device API. Полный контракт параметров и ответов: `internal/api/openapi.json`.

| Method | Path | Permission |
|---|---|---|
| GET | `/api/auth/me` | `""` |
| POST | `/api/auth/logout` | `""` |
| POST | `/api/auth/logout-all` | `""` |
| POST | `/api/auth/password` | `""` |
| GET | `/api/access/users` | `access.AccessManage` |
| POST | `/api/access/users` | `access.AccessManage` |
| PATCH | `/api/access/users/{id}` | `access.AccessManage` |
| POST | `/api/access/users/{id}/password` | `access.AccessManage` |
| GET | `/api/access/audit` | `access.AccessManage` |
| GET | `/api/health/rules` | `access.DataRead` |
| GET | `/api/reports/summary` | `access.DataRead` |
| GET | `/api/reports/snapshots` | `access.DataRead` |
| POST | `/api/reports/snapshots` | `access.ReportWrite` |
| GET | `/api/reports/snapshots/{id}` | `access.DataRead` |
| GET | `/api/agents/{id}/health` | `access.DataRead` |
| GET | `/api/devices/{id}/interfaces/health` | `access.DataRead` |
| GET | `/api/devices/{id}/interfaces/{interface_id}/health` | `access.DataRead` |
| GET | `/api/telephony/targets/{id}/health` | `access.DataRead` |
| GET | `/api/devices/{id}/health` | `access.DataRead` |
| GET | `/api/incidents` | `access.DataRead` |
| GET | `/api/incidents/{id}` | `access.DataRead` |
| GET | `/api/incidents/{id}/evidence` | `access.DataRead` |
| GET | `/api/incidents/{id}/context` | `access.DataRead` |
| POST | `/api/incidents/{id}/analysis/preview` | `access.DataRead` |
| POST | `/api/incidents/{id}/analysis` | `access.AnalysisWrite` |
| GET | `/api/incidents/{id}/analyses` | `access.DataRead` |
| GET | `/api/analyses/{analysis_id}` | `access.DataRead` |
| GET | `/api/incidents/{id}/transitions` | `access.DataRead` |
| POST | `/api/incidents/{id}/acknowledge` | `access.IncidentWrite` |
| POST | `/api/pki/bootstrap-tokens` | `access.AccessManage` |
| GET | `/api/pki/bootstrap-tokens` | `access.AccessManage` |
| POST | `/api/pki/bootstrap-tokens/{id}/revoke` | `access.AccessManage` |
| GET | `/api/pki/certificates` | `access.AccessManage` |
| POST | `/api/pki/certificates/{id}/revoke` | `access.AccessManage` |
| GET | `/api/pki/audit` | `access.AccessManage` |
| GET | `/api/commands` | `access.DataRead` |
| POST | `/api/commands` | `access.CommandWrite` |
| GET | `/api/commands/{id}` | `access.DataRead` |
| GET | `/api/commands/{id}/events` | `access.DataRead` |
| GET | `/api/commands/policy` | `access.DataRead` |
| POST | `/api/commands/{id}/rollback` | `access.CommandWrite` |
| GET | `/api/commands/{id}/snapshot` | `access.DataRead` |
| GET | `/api/snapshots` | `access.DataRead` |
| GET | `/api/agents` | `access.DataRead` |
| GET | `/api/availability/summary` | `access.DataRead` |
| GET | `/api/telephony/targets` | `access.DataRead` |
| GET | `/api/telephony/targets/{id}` | `access.DataRead` |
| GET | `/api/telephony/targets/{id}/history` | `access.DataRead` |
| GET | `/api/telephony/targets/{id}/monitoring` | `access.DataRead` |
| PUT | `/api/telephony/targets/{id}/monitoring` | `access.MonitoringWrite` |
| GET | `/api/telephony/summary` | `access.DataRead` |
| GET | `/api/logs` | `access.DataRead` |
| GET | `/api/logs/summary` | `access.DataRead` |
| GET | `/api/logs/{id}` | `access.DataRead` |
| GET | `/api/audit` | `access.DataRead` |
| GET | `/api/audit/events` | `access.DataRead` |
| GET | `/api/audit/events/{key}` | `access.DataRead` |
| GET | `/api/devices/{id}/logs` | `access.DataRead` |
| GET | `/api/devices/{id}/audit` | `access.DataRead` |
| GET | `/api/devices` | `access.DataRead` |
| GET | `/api/topology` | `access.DataRead` |
| GET | `/api/devices/{id}/topology` | `access.DataRead` |
| GET | `/api/devices/{id}` | `access.DataRead` |
| GET | `/api/devices/{id}/availability` | `access.DataRead` |
| GET | `/api/devices/{id}/availability/history` | `access.DataRead` |
| GET | `/api/devices/{id}/inventory` | `access.DataRead` |
| GET | `/api/devices/{id}/monitoring` | `access.DataRead` |
| PUT | `/api/devices/{id}/monitoring` | `access.MonitoringWrite` |
| GET | `/api/devices/{id}/metrics` | `access.DataRead` |
| GET | `/api/devices/{id}/telemetry` | `access.DataRead` |
| GET | `/api/devices/{id}/metrics/history` | `access.DataRead` |

Публичные маршруты: POST /api/auth/login, POST /api/auth/token, GET /healthz, GET /readyz, GET /openapi.json. Enrollment /enroll и /renew работают на отдельном PKI HTTPS listener.

Пустое permission означает обязательную аутентификацию без отдельного права. Cookie-мутации также требуют CSRF. Device ID может потребовать agent_id для однозначного выбора.
