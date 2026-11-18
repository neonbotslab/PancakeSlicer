import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from xml.etree import ElementTree
from zipfile import ZipFile


CUBE = """v 0 0 0
v 8 0 0
v 8 8 0
v 0 8 0
v 0 0 2
v 8 0 2
v 8 8 2
v 0 8 2
f 1 3 2
f 1 4 3
f 5 6 7
f 5 7 8
f 1 2 6
f 1 6 5
f 2 3 7
f 2 7 6
f 3 4 8
f 3 8 7
f 4 1 5
f 4 5 8
"""


def settings(**options):
    return [argument for key, value in options.items() for argument in ("--set", f"{key}={value}")]


def metadata(archive, member):
    plate = ElementTree.fromstring(archive.read(member)).find("plate")
    return plate, {row.attrib["key"]: row.attrib["value"] for row in plate.findall("metadata")}


def loaded(extruder, material="PLA", color="#D02020", external=False):
    return dict(extruder=extruder, type=material, color=color, external=external)


class NozzleMapping(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = tempfile.TemporaryDirectory()
        cls.addClassCleanup(directory.cleanup)
        cls.directory = Path(directory.name)
        cube = cls.directory / "cube.obj"
        cube.write_text(CUBE)
        first = dict(file=str(cube), extruder=1, x=140, y=140, plate=1)
        second = dict(first, extruder=2, x=160)
        left = dict(filament_map_mode="Manual", filament_map="1", filament_volume_map="1")
        cls.single_plate = cls.write_json("single.json", {
            "objects": [first],
            "plates": [{"index": 1, "settings": left}],
        })
        cls.dual_plate = cls.write_json("dual.json", {"objects": [first, second]})
        cls.project = cls.directory / "source.3mf"
        cls.run_tool("--plate", cls.single_plate, "--save-3mf", cls.project, *settings(**left))

    @classmethod
    def write_json(cls, name, data):
        path = cls.directory / name
        path.write_text(json.dumps(data))
        return path

    @classmethod
    def run_tool(cls, *arguments, success=True):
        command = [
            cls.tool, "--profiles", cls.profiles, "--datadir", cls.directory / "data",
            "--vendor", "BBL", "--printer", "Bambu Lab H2D 0.4 nozzle",
            "--process", "0.20mm Standard @BBL H2D", "--filament", "Generic PLA @BBL H2D",
            *settings(nonplanar_mode="off", enable_prime_tower=0),
            *arguments,
        ]
        result = subprocess.run(command, capture_output=True, text=True, timeout=60)
        if success and result.returncode != 0:
            raise AssertionError(f"Slice tool exited {result.returncode}\n{result.stdout}\n{result.stderr}")
        return result

    def slice(self, name, mode, *arguments, success=True):
        gcode = self.directory / f"{name}.gcode"
        package = self.directory / f"{name}.gcode.3mf"
        result = self.run_tool(
            "-o", gcode, "--gcode-3mf", package, *settings(filament_map_mode=mode), *arguments, success=success
        )
        if success:
            return package
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(gcode.exists())
        self.assertFalse(package.exists())
        self.assertIn("printer filament context", result.stderr)
        return result

    def assert_mapping(self, package, mode, mapping):
        values = [str(side) for side in mapping]
        nozzle_ids = [str(side - 1) for side in mapping]
        volumes = ["0"] * len(mapping)
        with ZipFile(package) as archive:
            project = json.loads(archive.read("Metadata/project_settings.config"))
            _, plate = metadata(archive, "Metadata/model_settings.config")
            slice_plate, slice_info = metadata(archive, "Metadata/slice_info.config")
            gcode = dict(
                line[2:].split(" = ", 1)
                for line in archive.read("Metadata/plate_1.gcode").decode().splitlines()
                if line.startswith("; filament_") and " = " in line
            )

        self.assertEqual(gcode["filament_map"], ",".join(values), "G-code uses the selected nozzle sides")
        self.assertEqual(gcode["filament_map_mode"], mode)
        self.assertEqual(gcode["filament_map_2"], ",".join(nozzle_ids))
        self.assertEqual(gcode["filament_nozzle_map"], ",".join(nozzle_ids))
        self.assertEqual(gcode["filament_volume_map"], ",".join(volumes))
        self.assertEqual(project["filament_map"], values, "project settings agree with G-code")
        self.assertEqual(project["filament_map_mode"], mode)
        self.assertEqual(project["filament_map_2"], nozzle_ids)
        self.assertEqual(project["filament_nozzle_map"], nozzle_ids)
        self.assertEqual(project["filament_volume_map"], volumes)
        self.assertEqual(plate["filament_map_mode"], mode)
        self.assertEqual(plate["filament_maps"], " ".join(values))
        self.assertEqual(plate["filament_volume_maps"], " ".join(volumes))
        self.assertEqual(slice_info["filament_maps"], " ".join(values))
        nozzles = {
            nozzle.attrib["id"]: (nozzle.attrib["extruder_id"], nozzle.attrib["volume_type"])
            for nozzle in slice_plate.findall("nozzle")
        }
        filaments = {
            int(filament.attrib["id"]): (filament.attrib["group_id"], filament.attrib["volume_type"])
            for filament in slice_plate.findall("filament")
        }
        self.assertEqual(nozzles, {nozzle_id: (side, "Standard") for nozzle_id, side in zip(nozzle_ids, values)})
        self.assertEqual(filaments, {index: (nozzle_id, "Standard") for index, nozzle_id in enumerate(nozzle_ids, start=1)})

    def test_imported_manual_right(self):
        with ZipFile(self.project) as archive:
            _, source_plate = metadata(archive, "Metadata/model_settings.config")
        self.assertEqual(source_plate["filament_map_mode"], "Manual")
        self.assertEqual(source_plate["filament_maps"], "1")
        self.assertEqual(source_plate["filament_volume_maps"], "1")
        package = self.slice("imported-right", "Manual", *settings(filament_map=2, filament_volume_map=0), self.project)
        self.assert_mapping(package, "Manual", [2])

    def test_imported_switch_to_auto_flush(self):
        mode = "Auto For Flush"
        package = self.slice("imported-flush", mode, self.project)
        self.assert_mapping(package, mode, [2])

    def test_two_manual_filaments(self):
        package = self.slice(
            "dual-manual", "Manual", "--plate", self.dual_plate, "--filament", "Generic PLA @BBL H2D",
            *settings(filament_map="2,1", filament_volume_map="0,0"),
        )
        self.assert_mapping(package, "Manual", [2, 1])

    def test_auto_match_requires_context(self):
        self.slice("missing-context", "Auto For Match", "--plate", self.single_plate, success=False)

    def test_loaded_colors_swap_nozzles(self):
        mode = "Auto For Match"
        stage_cache = self.directory / "stage-cache"
        for red_side in (0, 1):
            with self.subTest(red_side=red_side):
                context = self.write_json("colors.json", {
                    "ams_slots": [4, 4],
                    "loaded_filaments": [loaded(red_side), loaded(1 - red_side, color="#2050D0")],
                })
                package = self.slice(
                    f"colors-{red_side}", mode, "--plate", self.dual_plate, "--filament", "Generic PLA @BBL H2D",
                    "--colours", "#D02020,#2050D0", "--printer-filament-context", context,
                    "--stage-cache", stage_cache,
                )
                self.assert_mapping(package, mode, [red_side + 1, 2 - red_side])

    def test_loaded_material_beats_color(self):
        mode = "Auto For Match"
        context = self.write_json("materials.json", {
            "ams_slots": [0, 0],
            "loaded_filaments": [
                loaded(0, material="PETG", color="#D02020", external=True),
                loaded(1, color="#2050D0", external=True),
            ],
        })
        package = self.slice(
            "materials", mode, "--plate", self.dual_plate, "--filament", "Generic PETG @BBL H2D",
            "--colours", "#D02020,#2050D0", "--printer-filament-context", context, "--allow-mixed-temperatures",
        )
        self.assert_mapping(package, mode, [2, 1])

    def test_loaded_support_type_beats_color(self):
        mode = "Auto For Match"
        for support_type in ("PLA-S", "Sup.PLA"):
            with self.subTest(support_type=support_type):
                context = self.write_json("support.json", {
                    "ams_slots": [4, 4],
                    "loaded_filaments": [loaded(0, material=support_type), loaded(1, color="#2050D0")],
                })
                package = self.slice(
                    f"support-{support_type}", mode, "--plate", self.dual_plate,
                    "--filament", "Bambu Support For PLA @BBL H2D", "--colours", "#D02020,#2050D0",
                    "--printer-filament-context", context,
                )
                self.assert_mapping(package, mode, [2, 1])

    def test_invalid_context(self):
        cases = {
            "sides": {"ams_slots": [4]},
            "capacity": {"ams_slots": [4, 65]},
            "negative_capacity": {"ams_slots": [-1, 4]},
            "slot_type": {"ams_slots": [4.0, 4]},
            "ams_overflow": {"ams_slots": [0, 4]},
            "unknown_key": {"printer": "unused"},
            "no_filaments": {"loaded_filaments": []},
            "too_many_filaments": {"loaded_filaments": [loaded(0)] * 129},
            "invalid_side": {"loaded_filaments": [loaded(2)]},
            "invalid_color": {"loaded_filaments": [loaded(0, color="#QQQQQQ")]},
            "invalid_type": {"loaded_filaments": [loaded(0, material=" ")]},
            "long_type": {"loaded_filaments": [loaded(0, material="X" * 65)]},
            "invalid_external": {"loaded_filaments": [loaded(0, external=1)]},
            "extra_row_key": {"loaded_filaments": [dict(loaded(0), slot=1)]},
            "external_overflow": {"loaded_filaments": [loaded(0, external=True)] * 2},
        }
        for name, invalid in cases.items():
            with self.subTest(case=name):
                context = self.write_json("invalid.json", {"ams_slots": [4, 4], "loaded_filaments": [loaded(0)], **invalid})
                self.slice(
                    f"invalid-{name}", "Auto For Match", "--plate", self.single_plate,
                    "--printer-filament-context", context, success=False,
                )

    def test_malformed_context_file(self):
        for name, contents in {"malformed": "{", "oversize": " " * 65537}.items():
            with self.subTest(case=name):
                context = self.directory / f"{name}.json"
                context.write_text(contents)
                self.slice(
                    name, "Auto For Match", "--plate", self.single_plate,
                    "--printer-filament-context", context, success=False,
                )

    def test_help_lists_context_option(self):
        result = subprocess.run([self.tool, "--help"], capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--printer-filament-context FILE", result.stdout)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--tool", type=lambda value: Path(value).resolve(), required=True)
    parser.add_argument("--profiles", type=Path, default=Path(__file__).resolve().parents[2] / "resources" / "profiles")
    args, tests = parser.parse_known_args()
    NozzleMapping.tool = args.tool
    NozzleMapping.profiles = args.profiles
    unittest.main(argv=[__file__, *tests])
