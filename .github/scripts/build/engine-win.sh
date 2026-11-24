# Ninja with cl and ccache. ccache cannot cache /Zi objects, so PDBs are off.
configure() {
  export PATH="$(dirname "$(command -v cl)"):$PATH"
  cmake -S engine -B build-native -G 'Ninja Multi-Config' -DCMAKE_C_COMPILER=cl -DCMAKE_CXX_COMPILER=cl "${common[@]}" "${ccache[@]}" \
    -DSLIC3R_MSVC_PDB=OFF
}
