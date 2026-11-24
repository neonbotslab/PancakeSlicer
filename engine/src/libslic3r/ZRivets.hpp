#pragma once

#include "ExtrusionEntity.hpp"

namespace Slic3r {

class PrintObject;

class ZRivetPlugPath : public ExtrusionPath
{
public:
    using ExtrusionPath::ExtrusionPath;
    ExtrusionEntity *clone() const override { return new ZRivetPlugPath(*this); }
    ExtrusionEntity *clone_move() override { return new ZRivetPlugPath(std::move(*this)); }
    bool             can_reverse() const override { return false; }
};

void plan_z_rivets(PrintObject &object);
bool is_z_rivet_pour(const ExtrusionEntity &entity);

} // namespace Slic3r
