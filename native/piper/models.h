#pragma once
#include <stdexcept>
#include <string>

namespace piper {
struct model {
    const char *directory;
    const char *filename;
};

inline model model_for(const std::string &language) {
    if (language == "italian") return {"vits-piper-it_IT-miro-high", "it_IT-miro-high.onnx"};
    if (language == "english") return {"vits-piper-en_US-ryan-medium", "en_US-ryan-medium.onnx"};
    throw std::runtime_error("unsupported model language");
}
}
