# Unraid image build inputs

Build inputs of the deployed image `pancakeslicer-unraid:h2d-filaments-20261116`,
copied from its Docker build context (`<build-context-dir>/`).

- `Dockerfile`: thin layer on `pancakeslicer-unraid-base:20261116`
  (same image ID as `zimengxiong/pancakeslicer:latest`, upstream native engine unchanged).
  Copies the rebuilt Go server (`orca-webapp-server`), the built web app (`web/`) and the
  AGPL source archive (`source.tar.gz`) into the image.
- `build-inputs.json`: exact server, web and container build commands used.
- `dockerignore`: the `.dockerignore` of the source archive.

Deploys to the self-hosted instance are done by the operator, never from a dev box.
