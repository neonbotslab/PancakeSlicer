import argparse
import json
from pathlib import Path

server = Path(__file__).resolve().parent
printers = server.parent / "engine" / "resources" / "printers"
output = server / "bambu_auto_flow.generated.go"

parser = argparse.ArgumentParser()
parser.add_argument("--check", action="store_true")
args = parser.parse_args()

models = {}
for path in sorted(printers.glob("*.json")):
    config = json.loads(path.read_text(encoding="utf-8")).get("00.00.00.00", {})
    if config.get("print", {}).get("support_auto_flow_calibration") is True:
        name = config["display_name"].strip().removeprefix("Bambu Lab ")
        models[name.lower()] = name
        models[config["model_id"].strip().lower()] = name

entries = [(json.dumps(key) + ":", json.dumps(models[key])) for key in sorted(models)]
width = max((len(key) for key, _ in entries), default=0)
rows = "".join(f"\t{key:<{width}} {name},\n" for key, name in entries)
generated = "package main\n\nvar bambuAutoFlowModels = map[string]string{\n" + rows + "}\n"
if args.check:
    if output.read_text(encoding="utf-8") != generated:
        parser.exit(1, f"{output.name} is stale. Run python3 {Path(__file__).name}.\n")
else:
    output.write_text(generated, encoding="utf-8")
