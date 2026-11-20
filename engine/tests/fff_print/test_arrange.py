import argparse
from itertools import combinations
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


BLOCK = """v 0 0 0
v {size} 0 0
v {size} {size} 0
v 0 {size} 0
v 0 0 10
v {size} 0 10
v {size} {size} 10
v 0 {size} 10
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

BLOCKS = {
    "block-80": (80, 70, 70),
    "block-20": (20, 250, 60),
    "block-40": (40, 250, 200),
}
TOLERANCE = 1e-4


class Arrange(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = tempfile.TemporaryDirectory()
        cls.addClassCleanup(directory.cleanup)
        cls.directory = Path(directory.name)
        cls.catalog = cls.directory / "catalog"
        cls.run_tool("--flatten-catalog", cls.catalog, timeout=180)

        cls.profile_ids = {}
        for kind in ("prints", "filaments"):
            with (cls.catalog / f"{kind}.jsonl").open(encoding="utf-8") as records:
                next(records)
                for line in records:
                    product = json.loads(line)
                    for tuning in product["tunings"]:
                        cls.profile_ids[tuning["source"]["name"]] = product["id"]

        for name, (size, _, _) in BLOCKS.items():
            (cls.directory / f"{name}.obj").write_text(BLOCK.format(size=size), encoding="utf-8")

    @classmethod
    def run_tool(cls, *arguments, timeout=60):
        result = subprocess.run(
            [cls.tool, "--profiles", cls.profiles, "--datadir", cls.directory / "scratch-profile", *arguments],
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        if result.returncode != 0:
            raise AssertionError(f"Slice tool exited {result.returncode}\n{result.stdout}\n{result.stderr}")

    def arrange(self, label, order):
        objects = []
        for name in order:
            _, x, y = BLOCKS[name]
            objects.append({
                "file": f"{name}.obj",
                "name": name,
                "extruder": 1,
                "instances": [{"x": x, "y": y, "rot": 0, "plate": 1}],
            })
        plate = self.directory / f"{label}-plate.json"
        output = self.directory / f"{label}-arranged.json"
        plate.write_text(json.dumps({"objects": objects}), encoding="utf-8")
        self.run_tool(
            "--catalog", self.catalog,
            "--printer", "Bambu Lab H2D 0.4 nozzle",
            "--process", self.profile_ids["0.20mm Standard @BBL H2D"],
            "--filament", self.profile_ids["Generic PLA @BBL H2D"],
            "--set", "enable_prime_tower=0",
            "--plate", plate,
            "--arrange-only", output,
            "--arrange-no-rotation",
        )
        result = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(result["schema"], "orca.arrange.v1")
        self.assertEqual(result["plates"], 1)
        self.assertEqual(result["unplaced"], 0)
        self.assertCountEqual(
            [(obj["index"], obj["name"]) for obj in result["objects"]],
            enumerate(order),
        )

        placements = {}
        for obj in result["objects"]:
            self.assertEqual(len(obj["instances"]), 1, obj["name"])
            place = obj["instances"][0]
            self.assertEqual(place["index"], 0, obj["name"])
            self.assertEqual(place["plate"], 1, obj["name"])
            self.assertAlmostEqual(place["rot"], 0, delta=TOLERANCE, msg=obj["name"])
            placements[obj["name"]] = place
        return placements

    def test_arrange_keeps_block_placements(self):
        placements = self.arrange("insertion", BLOCKS)
        reordered = self.arrange("descending", ("block-80", "block-40", "block-20"))

        footprints = {}
        for name, place in placements.items():
            half = BLOCKS[name][0] / 2
            footprint = tuple((place[axis] - half, place[axis] + half) for axis in ("x", "y"))
            for (minimum, maximum), bed_max in zip(footprint, (350, 320)):
                self.assertGreaterEqual(minimum, -TOLERANCE, name)
                self.assertLessEqual(maximum, bed_max + TOLERANCE, name)
            footprints[name] = footprint

        for first, second in combinations(footprints, 2):
            with self.subTest(overlap=(first, second)):
                penetration = [
                    min(first_max, second_max) - max(first_min, second_min)
                    for (first_min, first_max), (second_min, second_max) in zip(
                        footprints[first], footprints[second]
                    )
                ]
                self.assertLessEqual(min(penetration), TOLERANCE, f"Footprint penetration in mm is {penetration}")

        for name, place in placements.items():
            for coordinate in ("x", "y", "rot"):
                with self.subTest(object=name, coordinate=coordinate):
                    self.assertAlmostEqual(place[coordinate], reordered[name][coordinate], delta=TOLERANCE)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--tool", type=lambda value: Path(value).resolve(), required=True)
    parser.add_argument("--profiles", type=Path, default=Path(__file__).resolve().parents[2] / "resources" / "profiles")
    args, tests = parser.parse_known_args()
    Arrange.tool = args.tool
    Arrange.profiles = args.profiles
    unittest.main(argv=[__file__, *tests])
