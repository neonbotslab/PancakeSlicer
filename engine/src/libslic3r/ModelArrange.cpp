#include "ModelArrange.hpp"

#include <cstddef>
#include <algorithm>
#include <array>
#include <iterator>
#include <libslic3r/Model.hpp>
#include <libslic3r/Geometry/ConvexHull.hpp>
#include <libslic3r/Print.hpp>
#include "Arrange.hpp"
#include <utility>
#include "Config.hpp"
#include <set>
#include <string>
#include "Point.hpp"
#include "PrintConfig.hpp"
#include "BoundingBox.hpp"
#include "ExPolygon.hpp"
#include "Polygon.hpp"

#include <limits>
#include <map>

namespace Slic3r {

namespace {

bool arrange_curved_mode(NonplanarMode mode)
{
    return curved_tops_mode(mode) || mode == NonplanarMode::ConformalLayers;
}

struct ArrangeReliefSettings
{
    NonplanarMode mode;
    double        max_angle;
    double        min_area;
};

void measure_nonplanar_relief(ModelInstance *instance, const DynamicPrintConfig &config, arrangement::ArrangePolygon &result)
{
    const auto *collision = config.option<ConfigOptionEnum<NonplanarCollisionCheck>>("nonplanar_collision_check");
    if (collision == nullptr || collision->value == NonplanarCollisionCheck::Off)
        return;

    const ModelObject *object = instance->get_object();
    DynamicPrintConfig effective = config;
    effective.apply(object->config.get(), true);
    if (effective.opt_enum<RotaryLayout>("nonplanar_kinematics") != RotaryLayout::None || effective.opt_bool("belt_printer"))
        return;
    const bool native_support = effective.opt_bool("enable_support") && effective.opt_enum<SupportMaterialStyle>("support_style") == smsNative;
    const NonplanarMode automatic = native_support ? NonplanarMode::Off : NonplanarMode::NonplanarTop;
    ArrangeReliefSettings base { effective.opt_enum<NonplanarMode>("nonplanar_mode"),
                                 effective.opt_float("nonplanar_curved_max_angle"), effective.opt_float("nonplanar_curved_min_area") };
    if (base.mode == NonplanarMode::Auto)
        base.mode = automatic;

    auto apply_range = [](ArrangeReliefSettings &settings, const DynamicPrintConfig &config, NonplanarMode automatic) {
        if (const auto *mode = config.option<ConfigOptionEnum<NonplanarMode>>("nonplanar_mode")) {
            settings.mode = mode->value;
            if (settings.mode == NonplanarMode::Auto)
                settings.mode = automatic;
        }
        if (const auto *angle = config.option<ConfigOptionFloat>("nonplanar_curved_max_angle"))
            settings.max_angle = angle->value;
        if (const auto *area = config.option<ConfigOptionFloat>("nonplanar_curved_min_area"))
            settings.min_area = area->value;
    };
    auto at = [&](double z) {
        ArrangeReliefSettings out = base;
        for (const auto &[range, config] : object->layer_config_ranges)
            if (z >= range.first && z < range.second)
                apply_range(out, config.get(), automatic);
        return out;
    };
    auto relevant = [&](NonplanarMode mode) { return arrange_curved_mode(mode == NonplanarMode::Auto ? automatic : mode); };
    if (!relevant(base.mode)) {
        bool any = false;
        for (const auto &[range, config] : object->layer_config_ranges) {
            ArrangeReliefSettings settings = base;
            apply_range(settings, config.get(), automatic);
            any |= relevant(settings.mode);
        }
        if (!any)
            return;
    }

    std::vector<double> range_cuts;
    for (const auto &[range, config] : object->layer_config_ranges) {
        range_cuts.push_back(range.first);
        range_cuts.push_back(range.second);
    }
    std::sort(range_cuts.begin(), range_cuts.end());
    range_cuts.erase(std::unique(range_cuts.begin(), range_cuts.end()), range_cuts.end());

    static constexpr double inf = std::numeric_limits<double>::infinity();
    struct Facet { double low = inf, high = -inf, area = 0., min_area = inf; };
    double start_z = inf;
    const Transform3d matrix = instance->get_matrix();
    for (const ModelVolume *volume : object->volumes) {
        if (!volume->is_model_part())
            continue;
        const Transform3d transform = matrix * volume->get_matrix();
        const double winding = transform.linear().determinant() < 0. ? -1. : 1.;
        const indexed_triangle_set &mesh = volume->mesh().its;
        std::vector<Facet> facets;
        std::vector<size_t> parent;
        auto root = [&](size_t i) {
            while (parent[i] != i) {
                parent[i] = parent[parent[i]];
                i = parent[i];
            }
            return i;
        };
        auto join = [&](size_t a, size_t b) {
            parent[root(a)] = root(b);
        };
        std::multimap<std::pair<int, int>, size_t> edges;
        for (const stl_triangle_vertex_indices &face : mesh.indices) {
            const Vec3d a = transform * mesh.vertices[face[0]].cast<double>();
            const Vec3d b = transform * mesh.vertices[face[1]].cast<double>();
            const Vec3d c = transform * mesh.vertices[face[2]].cast<double>();
            const Vec3d normal = (b - a).cross(c - a) * winding;
            const double length = normal.norm();
            if (length <= 0. || normal.z() <= 0.)
                continue;
            const double nz = normal.z() / length;
            const std::array<double, 3> vertex_z { a.z(), b.z(), c.z() };
            auto heights = vertex_z;
            std::sort(heights.begin(), heights.end());
            const double low = heights[0], middle = heights[1], high = heights[2];
            const size_t first = facets.size();
            // The side of the cut containing one original vertex is a similar triangle.
            auto area_below = [&](double z) {
                if (z <= low)
                    return 0.;
                if (z >= high)
                    return 1.;
                if (z <= middle)
                    return (z - low) / (middle - low) * (z - low) / (high - low);
                return 1. - (high - z) / (high - middle) * (high - z) / (high - low);
            };
            auto add_facet = [&](double from, double to) {
                const ArrangeReliefSettings settings = at(0.5 * (from + to));
                if (!relevant(settings.mode))
                    return;
                const double angle = settings.mode == NonplanarMode::ConformalLayers ? std::clamp(settings.max_angle, 1., 45.) :
                                                                                       std::clamp(settings.max_angle, 0., 45.);
                if (nz + 1e-9 < std::cos(Geometry::deg2rad(angle)))
                    return;
                const double area = 0.5 * length * (low == high ? 1. : area_below(to) - area_below(from));
                if (area <= 0.)
                    return;
                const size_t i = facets.size();
                parent.push_back(i);
                if (i > first && facets.back().high == from)
                    join(i, i - 1);
                facets.push_back({ from, to, area, std::max(0., settings.min_area) });
                for (int k = 0; k < 3; ++k) {
                    const int next = (k + 1) % 3;
                    const auto [edge_low, edge_high] = std::minmax(vertex_z[k], vertex_z[next]);
                    const double edge_from = std::max(from, edge_low), edge_to = std::min(to, edge_high);
                    if (edge_from > edge_to || (edge_from == edge_to && edge_low != edge_high))
                        continue;
                    const auto edge = std::minmax(face[k], face[next]);
                    const auto [begin, end] = edges.equal_range(edge);
                    for (auto it = begin; it != end; ++it) {
                        const size_t j = it->second;
                        const double shared_from = std::max(edge_from, facets[j].low);
                        const double shared_to = std::min(edge_to, facets[j].high);
                        if (shared_from < shared_to || (shared_from == shared_to && edge_low == edge_high))
                            join(i, j);
                    }
                    edges.emplace_hint(end, edge, i);
                }
            };
            double from = low;
            for (auto it = std::upper_bound(range_cuts.begin(), range_cuts.end(), low); it != range_cuts.end() && *it < high; ++it) {
                add_facet(from, *it);
                from = *it;
            }
            add_facet(from, high);
        }

        std::vector<Facet> patches(facets.size());
        for (size_t i = 0; i < facets.size(); ++i) {
            Facet &patch = patches[root(i)];
            patch.low = std::min(patch.low, facets[i].low);
            patch.high = std::max(patch.high, facets[i].high);
            patch.area += facets[i].area;
            patch.min_area = std::min(patch.min_area, facets[i].min_area);
        }
        for (const Facet &patch : patches) {
            const double relief = patch.high - patch.low;
            if (patch.area + EPSILON >= patch.min_area && relief >= 0.03) {
                result.nonplanar_relief = std::max(result.nonplanar_relief, relief);
                start_z = std::min(start_z, patch.low);
            }
        }
    }
    if (result.nonplanar_relief > 0.)
        result.nonplanar_start_z = start_z;
}

}

arrangement::ArrangePolygons get_arrange_polys(const Model &model, ModelInstancePtrs &instances)
{
    size_t count = 0;
    for (auto obj : model.objects) count += obj->instances.size();

    ArrangePolygons input;
    input.reserve(count);
    instances.clear(); instances.reserve(count);
    ArrangePolygon ap;
    for (ModelObject *mo : model.objects)
        for (ModelInstance *minst : mo->instances) {
            minst->get_arrange_polygon(&ap);
            ap.bed_idx = 0;
            input.emplace_back(ap);
            instances.emplace_back(minst);
        }

    return input;
}

bool apply_arrange_polys(ArrangePolygons &input, ModelInstancePtrs &instances, VirtualBedFn vfn)
{
    bool ret = true;

    for(size_t i = 0; i < input.size(); ++i) {
        if (input[i].bed_idx != 0) { ret = false; if (vfn) vfn(input[i]); }
        if (input[i].bed_idx >= 0)
            instances[i]->apply_arrange_result(input[i].translation.cast<double>(),
                                               input[i].rotation);
    }

    return ret;
}

Slic3r::arrangement::ArrangePolygon get_arrange_poly(const Model &model)
{
    ArrangePolygon ap;
    Points &apts = ap.poly.contour.points;
    for (const ModelObject *mo : model.objects)
        for (const ModelInstance *minst : mo->instances) {
            ArrangePolygon obj_ap;
            minst->get_arrange_polygon(&obj_ap);
            ap.poly.contour.rotate(obj_ap.rotation);
            ap.poly.contour.translate(obj_ap.translation.x(), obj_ap.translation.y());
            const Points &pts = obj_ap.poly.contour.points;
            std::copy(pts.begin(), pts.end(), std::back_inserter(apts));
        }

    apts = std::move(Geometry::convex_hull(apts).points);
    return ap;
}

void duplicate(Model &model, Slic3r::arrangement::ArrangePolygons &copies, VirtualBedFn vfn)
{
    for (ModelObject *o : model.objects) {
        ModelInstancePtrs instances = o->instances;
        o->instances.clear();
        for (const ModelInstance *i : instances) {
            for (arrangement::ArrangePolygon &ap : copies) {
                if (ap.bed_idx != 0) vfn(ap);
                ModelInstance *instance = o->add_instance(*i);
                Vec2d pos = unscale(ap.translation);
                instance->set_offset(instance->get_offset() + to_3d(pos, 0.));
            }
        }
        o->invalidate_bounding_box();
    }
}

void duplicate_objects(Model &model, size_t copies_num)
{
    for (ModelObject *o : model.objects) {
        ModelInstancePtrs instances = o->instances;
        for (const ModelInstance *i : instances)
            for (size_t k = 2; k <= copies_num; ++ k)
                o->add_instance(*i);
    }
}

template<class T>
arrangement::ArrangePolygon get_arrange_poly(T obj, const Slic3r::DynamicPrintConfig& config)
{
    ArrangePolygon ap = obj.get_arrange_polygon(config);
    ap.bed_idx = 0;
    ap.setter = [obj](const ArrangePolygon& p) {
        if (p.is_arranged()) {
            Vec2d t = p.translation.cast<double>();
            T{ obj }.apply_arrange_result(t, p.rotation, p.itemid);
        }
    };

    return ap;
}

template<>
arrangement::ArrangePolygon get_arrange_poly(ModelInstance* inst, const Slic3r::DynamicPrintConfig& config)
{
    return get_arrange_poly(PtrWrapper{ inst },config);
}

ArrangePolygon get_instance_arrange_poly(ModelInstance* instance, const Slic3r::DynamicPrintConfig& config)
{
    ArrangePolygon ap = get_arrange_poly(PtrWrapper{ instance }, config);

    if (config.has("curr_bed_type")) {
        ap.bed_temp = 0;
        ap.first_bed_temp = 0;
        BedType curr_bed_type = config.opt_enum<BedType>("curr_bed_type");

        const ConfigOptionInts* bed_opt = config.option<ConfigOptionInts>(get_bed_temp_key(curr_bed_type));
        if (bed_opt != nullptr)
            ap.bed_temp = bed_opt->get_at(ap.extrude_ids.front()-1);

        const ConfigOptionInts* bed_opt_1st_layer = config.option<ConfigOptionInts>(get_bed_temp_1st_layer_key(curr_bed_type));
        if (bed_opt_1st_layer != nullptr)
            ap.first_bed_temp = bed_opt_1st_layer->get_at(ap.extrude_ids.front()-1);
    }

    if (config.has("nozzle_temperature"))
        ap.print_temp = config.opt_int("nozzle_temperature", ap.extrude_ids.front() - 1);
    if (config.has("nozzle_temperature_initial_layer"))
        ap.first_print_temp = config.opt_int("nozzle_temperature_initial_layer", ap.extrude_ids.front() - 1);

    if (config.has("temperature_vitrification")) {
        ap.vitrify_temp = config.opt_int("temperature_vitrification", ap.extrude_ids.front() - 1);
    }

    auto* filament_types_opt = dynamic_cast<const ConfigOptionStrings*>(config.option("filament_type"));
    if (filament_types_opt) {
        std::set<int> filament_temp_types;
        for (auto i : ap.extrude_ids) {
            std::string type_str = filament_types_opt->get_at(i-1);
            int temp_type = Print::get_filament_temp_type(type_str);
            filament_temp_types.insert(temp_type);
        }
        ap.filament_temp_type = Print::get_compatible_filament_type(filament_temp_types);
    }

    auto obj = instance->get_object();

    ap.brim_width = 1.0;
    auto supp_type_ptr = obj->get_config_value<ConfigOptionBool>(config, "enable_support");
    auto support_type_ptr = obj->get_config_value<ConfigOptionEnum<SupportType>>(config, "support_type");
    auto support_type = support_type_ptr->value;
    auto enable_support = supp_type_ptr->getBool();
    int support_int = support_type_ptr->getInt();

    if (enable_support && (support_type == stNormalAuto || support_type == stNormal))
        ap.brim_width = 6.0;
    else if (enable_support) {
        ap.brim_width = 24.0;
        ap.has_tree_support = true;
    }

    const BoundingBoxf3 box = obj->instance_convex_hull_bounding_box(instance);
    ap.height = box.size().z();
    ap.occupied_top_z = box.max.z();
    measure_nonplanar_relief(instance, config, ap);
    ap.name = obj->name;
    return ap;
}

}
