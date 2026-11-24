#include "ZRivets.hpp"

#include "BoundingBox.hpp"
#include "ClipperUtils.hpp"
#include "ExPolygon.hpp"
#include "ExtrusionEntity.hpp"
#include "ExtrusionEntityCollection.hpp"
#include "Layer.hpp"
#include "Point.hpp"
#include "Polygon.hpp"
#include "Polyline.hpp"
#include "Print.hpp"
#include "PrintConfig.hpp"
#include "Surface.hpp"
#include "SurfaceCollection.hpp"
#include "libslic3r.h"

#include <tbb/blocked_range.h>
#include <tbb/parallel_for.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <numeric>
#include <utility>
#include <vector>

namespace Slic3r {

namespace {

constexpr double pour_overfill       = 1.05;
constexpr double min_cavity_mm2      = 0.5;
constexpr double max_cavity_nozzles2 = 94.;

struct Cavity
{
    ExPolygon   shape;
    BoundingBox box;
    double      area;
    bool        pourable;
};

void append_bead_outlines(const ExtrusionEntity &entity, Polygons &out)
{
    if (const auto *collection = dynamic_cast<const ExtrusionEntityCollection *>(&entity)) {
        for (const ExtrusionEntity *child : collection->entities)
            append_bead_outlines(*child, out);
    } else if (const auto *path = dynamic_cast<const ExtrusionPath *>(&entity)) {
        if (path->role() == erInternalInfill)
            polygons_append(out, offset(path->polyline.to_polyline(), float(scale_(0.5 * path->width))));
    } else if (const auto *multi = dynamic_cast<const ExtrusionMultiPath *>(&entity)) {
        for (const ExtrusionPath &p : multi->paths)
            append_bead_outlines(p, out);
    } else if (const auto *loop = dynamic_cast<const ExtrusionLoop *>(&entity)) {
        for (const ExtrusionPath &p : loop->paths)
            append_bead_outlines(p, out);
    }
}

std::vector<Cavity> layer_cavities(const LayerRegion &layerm, double max_area)
{
    const ExPolygons sparse = union_ex(to_polygons(layerm.fill_surfaces.filter_by_type(stInternal)));
    if (sparse.empty())
        return {};
    Polygons beads;
    for (const ExtrusionEntity *fill : layerm.fills.entities)
        append_bead_outlines(*fill, beads);
    std::vector<Cavity> out;
    for (ExPolygon &shape : diff_ex(sparse, beads)) {
        const double area = shape.area() * SCALING_FACTOR * SCALING_FACTOR;
        BoundingBox  box  = get_extents(shape.contour);
        out.push_back({ std::move(shape), box, area, area >= min_cavity_mm2 && area <= max_area });
    }
    return out;
}

bool overlaps(const Cavity &a, const Cavity &b)
{
    return a.box.overlap(b.box) && !intersection_ex(a.shape, b.shape).empty();
}

struct DisjointSets
{
    std::vector<size_t> parent;
    explicit DisjointSets(size_t n) : parent(n) { std::iota(parent.begin(), parent.end(), 0); }
    size_t find(size_t i) { return parent[i] == i ? i : parent[i] = find(parent[i]); }
    void   unite(size_t a, size_t b) { parent[find(a)] = find(b); }
};

} // namespace

void plan_z_rivets(PrintObject &object)
{
    LayerPtrs &layers = object.layers();
    for (size_t region_id = 0; region_id < object.num_printing_regions(); ++region_id) {
        const PrintRegionConfig &cfg = object.printing_region(region_id).config();
        if (!cfg.z_rivets.value || layers.size() < 3)
            continue;
        const int    span     = std::max(2, cfg.z_rivet_span.value);
        const double nozzle   = object.print()->config().nozzle_diameter.get_at(std::max(1u, object.printing_region(region_id).extruder(frInfill)) - 1);
        const double max_area = max_cavity_nozzles2 * nozzle * nozzle;

        std::vector<std::vector<Cavity>> cavities(layers.size());
        tbb::parallel_for(tbb::blocked_range<size_t>(0, layers.size()), [&](const tbb::blocked_range<size_t> &range) {
            for (size_t l = range.begin(); l < range.end(); ++l)
                if (const LayerRegion *layerm = layers[l]->get_region(int(region_id)))
                    cavities[l] = layer_cavities(*layerm, max_area);
        });

        std::vector<size_t> first_id(layers.size() + 1, 0);
        for (size_t l = 0; l < layers.size(); ++l)
            first_id[l + 1] = first_id[l] + cavities[l].size();
        DisjointSets      cells(first_id.back());
        std::vector<char> leaks(first_id.back(), 0);
        std::vector<std::vector<std::pair<size_t, size_t>>> links(layers.size());
        tbb::parallel_for(tbb::blocked_range<size_t>(1, layers.size()), [&](const tbb::blocked_range<size_t> &range) {
            for (size_t l = range.begin(); l < range.end(); ++l)
                for (size_t i = 0; i < cavities[l].size(); ++i)
                    for (size_t j = 0; j < cavities[l - 1].size(); ++j)
                        if (overlaps(cavities[l][i], cavities[l - 1][j]))
                            links[l].emplace_back(j, i);
        });
        for (size_t l = 1; l < layers.size(); ++l)
            for (const auto &[j, i] : links[l]) {
                const size_t above = first_id[l] + i, below = first_id[l - 1] + j;
                if (cavities[l][i].pourable && cavities[l - 1][j].pourable)
                    cells.unite(above, below);
                else if (cavities[l][i].pourable)
                    leaks[above] = 1;
            }
        std::vector<char> column_leaks(first_id.back(), 0);
        for (size_t n = 0; n < leaks.size(); ++n)
            if (leaks[n])
                column_leaks[cells.find(n)] = 1;

        std::vector<std::vector<std::pair<size_t, size_t>>> columns(first_id.back());
        for (size_t l = 0; l < layers.size(); ++l)
            for (size_t i = 0; i < cavities[l].size(); ++i)
                if (cavities[l][i].pourable && !column_leaks[cells.find(first_id[l] + i)])
                    columns[cells.find(first_id[l] + i)].emplace_back(l, i);
        size_t stride = size_t(span / 2 + 1);
        while (std::gcd(stride, size_t(span)) != 1)
            ++stride;
        size_t count = 0;
        for (const auto &column : columns) {
            if (column.size() < 2)
                continue;
            const size_t lo    = column.front().first;
            const size_t first = 2 + (count++ * stride) % size_t(span);
            for (size_t begin = 0; begin < column.size();) {
                const size_t window_top = column[begin].first < lo + first ? lo + first - 1 :
                                                                             lo + first - 1 + ((column[begin].first - lo - first) / span + 1) * span;
                size_t end = begin;
                while (end < column.size() && column[end].first <= window_top)
                    ++end;
                DisjointSets pieces(end - begin);
                for (size_t a = begin; a < end; ++a)
                    for (size_t b = a + 1; b < end && column[b].first <= column[a].first + 1; ++b)
                        if (column[b].first == column[a].first + 1 &&
                            overlaps(cavities[column[a].first][column[a].second], cavities[column[b].first][column[b].second]))
                            pieces.unite(a - begin, b - begin);
                std::vector<double> volume(end - begin, 0.);
                std::vector<size_t> top(end - begin, SIZE_MAX), bottom(end - begin, SIZE_MAX);
                for (size_t a = begin; a < end; ++a) {
                    const size_t root  = pieces.find(a - begin);
                    const auto [l, i]  = column[a];
                    volume[root]      += cavities[l][i].area * layers[l]->height;
                    if (top[root] == SIZE_MAX || l > column[top[root]].first ||
                        (l == column[top[root]].first && cavities[l][i].area > cavities[l][column[top[root]].second].area))
                        top[root] = a;
                    if (bottom[root] == SIZE_MAX)
                        bottom[root] = a;
                }
                for (size_t k = 0; k < end - begin; ++k) {
                    if (top[k] == SIZE_MAX)
                        continue;
                    const auto [l, i]  = column[top[k]];
                    const Cavity &cell = cavities[l][i];
                    const Point centre = cell.shape.contour.centroid();
                    if (!cell.shape.contains(centre))
                        continue;
                    const size_t low   = column[bottom[k]].first;
                    const double depth = layers[l]->print_z - layers[low]->print_z + layers[low]->height;
                    const double len   = std::min(1., 0.5 * std::sqrt(cell.area));
                    auto        *pour  = new ZRivetPlugPath(erInternalInfill, volume[k] * pour_overfill / len,
                                                            float(2. * std::sqrt(cell.area / M_PI)), float(depth));
                    pour->polyline     = Polyline3(Polyline(centre - Point::new_scale(0.5 * len, 0.), centre + Point::new_scale(0.5 * len, 0.)));
                    auto *coll         = new ExtrusionEntityCollection();
                    coll->no_sort      = true;
                    coll->entities.push_back(pour);
                    layers[l]->get_region(int(region_id))->fills.entities.push_back(coll);
                }
                begin = end;
            }
        }
    }
}

bool is_z_rivet_pour(const ExtrusionEntity &entity)
{
    const auto *collection = dynamic_cast<const ExtrusionEntityCollection *>(&entity);
    return collection != nullptr && collection->entities.size() == 1 && dynamic_cast<const ZRivetPlugPath *>(collection->entities.front()) != nullptr;
}

} // namespace Slic3r
