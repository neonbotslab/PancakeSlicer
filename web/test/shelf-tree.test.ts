import assert from 'node:assert/strict';
import { test } from 'node:test';
import { descendants, makeTree, parseEntry, searchTree } from '../src/shelf/tree.ts';

const calibration = parseEntry({ id: 'folder:calibration', parent: '', name: 'Calibration' }, 'folder');
const towers = parseEntry({ id: 'folder:towers', parent: calibration.id, name: 'Towers' }, 'folder');
const archive = parseEntry({ id: 'folder:archive', parent: towers.id, name: 'Archive' }, 'folder');
const drafts = parseEntry({ id: 'folder:drafts', parent: archive.id, name: 'Drafts' }, 'folder');
const retract = parseEntry({ id: 'model:retract', parent: towers.id, name: 'Retract.stl' }, 'model');
const tower = parseEntry({ id: 'model:tower', parent: archive.id, name: 'Temperature Tower.stl' }, 'model');
const tree = makeTree(1, [calibration, towers, archive, drafts, retract, tower]);

test('shelf search matches folder and model names and returns their parent paths', () => {
  assert.deepEqual(searchTree(tree, 'TOWER'.toLocaleLowerCase()), [
    { entry: towers, path: 'Calibration' },
    { entry: tower, path: 'Calibration / Towers / Archive' },
  ]);
  assert.deepEqual(searchTree(tree, 'retract'), [{ entry: retract, path: 'Calibration / Towers' }]);
  assert.deepEqual(searchTree(tree, 'calibration'), [{ entry: calibration, path: 'Shelf' }]);
});

test('folder descendant counts include models and folders at every depth', () => {
  assert.equal(descendants(tree, towers.id).length, 4);
  assert.equal(descendants(tree, archive.id).length, 2);
  assert.equal(descendants(tree, drafts.id).length, 0);
});
