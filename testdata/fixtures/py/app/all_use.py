from .all_pkg import all_hidden, all_listed


def use_listed() -> int:
    # `all_pkg/impl.py` lists this name in __all__, so the wildcard re-export
    # binds it and the import walk resolves at py.import.star (#110).
    return all_listed(1)


def use_hidden() -> int:
    # The same module omits this name from __all__, so `from .impl import *`
    # never binds it: the reference stays pending instead of resolving (#110).
    return all_hidden(2)
