[
  .items[]
  | select(.kind == "StringSecret" or .kind == "BasicAuth" or .kind == "SSHKeyPair" or .kind == "Secret")
  | select(.metadata.name | startswith("smoke-"))
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
              controller: (.controller // false)
            }
        ]
        | sort_by([.apiVersion, .kind, .name, .uid])
      )
    }
]
| sort_by([.kind, .name])
