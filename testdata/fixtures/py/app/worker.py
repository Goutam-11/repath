import os
from . import util
from .util import parse as to_int


def measure(text: str) -> int:
    value = to_int(text)
    bounded = util.clamp(value, 0, 10)
    return bounded + len(text)


def touch(path: str) -> None:
    os.path.exists(path)
