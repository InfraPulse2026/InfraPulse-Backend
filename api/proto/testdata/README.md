`p2-descriptor.pb` is the unmodified Backend P2 `FileDescriptorSet` captured
before synchronizing to the canonical Agent P3 schema. It was generated with
protoc 33.5 using `--descriptor_set_out` (without `--include_imports`).

`TestP3PreservesP2WireContract` compares all existing message field numbers,
types, presence, oneof membership, enum numeric values, and streaming service
definitions against this baseline. The fixture is intentionally frozen: do not
regenerate it when adding fields to the canonical contract.

`stage9f-descriptor.pb` freezes the complete generated Stage9F descriptor at B0
(2026-10-10), including the existing application ACK, PKI and rollback fields.
It was serialized from `protodesc.ToFileDescriptorProto` as a one-file
FileDescriptorSet. `TestV2PreservesStage9FWireContract` applies the same checks.
Do not regenerate either fixture as part of B1 or later phases.
