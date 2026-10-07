`p2-descriptor.pb` is the unmodified Backend P2 `FileDescriptorSet` captured
before synchronizing to the canonical Agent P3 schema. It was generated with
protoc 33.5 using `--descriptor_set_out` (without `--include_imports`).

`TestP3PreservesP2WireContract` compares all existing message field numbers,
types, presence, oneof membership, enum numeric values, and streaming service
definitions against this baseline. The fixture is intentionally frozen: do not
regenerate it when adding fields to the canonical contract.
