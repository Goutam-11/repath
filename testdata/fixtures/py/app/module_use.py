import app.util as util_module


def measure() -> int:
    return util_module.clamp(1, 0, 2)


import app.util


def measure_plain() -> int:
    return app.util.clamp(1, 0, 2)
