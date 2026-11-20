from app.star_cycle import cycle_helper


def use_cycle() -> int:
    # alpha and beta star-import each other; the walk must terminate once.
    return cycle_helper(1)
