(.items | [
  .[]
  | select(
      (.kind == "StringSecret" and .metadata.name == "smoke-string") or
      (.kind == "BasicAuth" and .metadata.name == "smoke-basic") or
      (.kind == "SSHKeyPair" and .metadata.name == "smoke-ssh")
    )
  | .metadata.uid
]) as $fixture_cr_uids
| [
  .items[]
  | select(
      (.kind == "StringSecret" and .metadata.name == "smoke-string") or
      (.kind == "BasicAuth" and .metadata.name == "smoke-basic") or
      (.kind == "SSHKeyPair" and .metadata.name == "smoke-ssh") or
      (
        .kind == "Secret" and
        (
          (.metadata.name == "smoke-string" or
           .metadata.name == "smoke-basic" or
           .metadata.name == "smoke-ssh" or
           .metadata.name == "smoke-annotation") or
          any(.metadata.ownerReferences[]?;
            (.uid as $owner_uid | $fixture_cr_uids | index($owner_uid) != null))
        )
      )
    )
  | {
      kind,
      name: .metadata.name,
      uid: .metadata.uid,
      owners: (
        [
          (.metadata.ownerReferences // [])[]
          | {
              apiVersion,
              kind,
              name,
              uid,
              controller: (.controller // false),
              blockOwnerDeletion: (.blockOwnerDeletion // false)
            }
        ]
        | sort_by([.apiVersion, .kind, .name, .uid])
      )
    }
]
| sort_by([.kind, .name])
