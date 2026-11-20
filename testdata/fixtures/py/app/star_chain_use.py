from app.star_chain import chain_helper


def use_chain() -> int:
    # Two wildcard hops: app/star_chain/__init__.py -> middle.py -> leaf.py.
    return chain_helper(2)
