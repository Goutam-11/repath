pub struct Base {
    pub id: u32,
}

pub struct User {
    pub name: String,
}

impl User {
    pub fn new(name: String) -> Self {
        User { name }
    }

    pub fn greet(&self) -> String {
        "hi".to_string()
    }
}

pub trait Identified {
    fn identify(&self) -> u32;
}

pub struct Cross {
    pub n: u32,
}
